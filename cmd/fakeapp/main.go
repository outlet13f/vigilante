// Command fakeapp is a demo service for the end-to-end rollback demo. It
// serves /health and /api, writes an nginx-style access log and an application
// log, generates its own traffic, and can be "deployed" to another version at
// runtime. Versions listed in -bad misbehave: ~10% 5xx, 4-6x latency, and
// OutOfMemoryError / exception lines in the application log.
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

type app struct {
	mu      sync.RWMutex
	version string
	bad     []string
	access  *os.File
	applog  *os.File
	logMu   sync.Mutex
}

func (a *app) current() (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.version, slices.Contains(a.bad, a.version)
}

func (a *app) logApp(format string, args ...any) {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	fmt.Fprintf(a.applog, time.Now().Format("2006-01-02 15:04:05.000")+" "+format+"\n", args...)
}

func (a *app) logAccess(r *http.Request, status int, d time.Duration) {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	fmt.Fprintf(a.access, "%s - - [%s] \"%s %s %s\" %d %d \"-\" \"%s\" %.3f\n",
		strings.Split(r.RemoteAddr, ":")[0], time.Now().Format("02/Jan/2006:15:04:05 -0700"),
		r.Method, r.URL.RequestURI(), r.Proto, status, 42, r.UserAgent(), d.Seconds())
}

func (a *app) work(w http.ResponseWriter, r *http.Request, failRatio float64) {
	start := time.Now()
	_, bad := a.current()
	base := 4 + rand.IntN(8) // 4-11ms
	if bad {
		base = 25 + rand.IntN(45) // 25-69ms
	}
	time.Sleep(time.Duration(base) * time.Millisecond)
	status := 200
	if bad && rand.Float64() < failRatio {
		status = 500
		a.logApp("ERROR [http-nio-8080-exec-%d] o.e.OrderController - Request failed: java.lang.IllegalStateException: pool closed\n\tat com.example.order.Repo.find(Repo.java:42)", rand.IntN(200))
	}
	w.WriteHeader(status)
	fmt.Fprintf(w, "ok %d\n", status)
	a.logAccess(r, status, time.Since(start))
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "listen address")
	version := flag.String("version", "v1", "initial version")
	bad := flag.String("bad", "v2", "comma-separated versions that misbehave")
	access := flag.String("access-log", "access.log", "access log path")
	applog := flag.String("app-log", "app.log", "application log path")
	rps := flag.Int("load", 40, "self-generated requests per second to /api (0 = none)")
	flag.Parse()

	open := func(p string) *os.File {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatal(err)
		}
		return f
	}
	a := &app{version: *version, bad: strings.Split(*bad, ","), access: open(*access), applog: open(*applog)}
	a.logApp("INFO  Started fakeapp %s", *version)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		_, isBad := a.current()
		d := 2 + rand.IntN(6)
		if isBad {
			d = 20 + rand.IntN(40)
		}
		time.Sleep(time.Duration(d) * time.Millisecond)
		fmt.Fprintln(w, `{"status":"UP"}`)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { a.work(w, r, 0.10) })
	mux.HandleFunc("POST /admin/deploy", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query().Get("version")
		if v == "" {
			http.Error(w, "version required", 400)
			return
		}
		a.mu.Lock()
		old := a.version
		a.version = v
		a.mu.Unlock()
		a.logApp("INFO  Deployed %s (was %s)", v, old)
		log.Printf("deployed %s (was %s)", v, old)
		fmt.Fprintf(w, "deployed %s\n", v)
	})
	mux.HandleFunc("GET /admin/version", func(w http.ResponseWriter, r *http.Request) {
		v, _ := a.current()
		if exp := r.URL.Query().Get("expect"); exp != "" && exp != v {
			http.Error(w, "running "+v+", expected "+exp, http.StatusConflict)
			return
		}
		fmt.Fprintln(w, v)
	})

	// Background noise of a sick JVM.
	go func() {
		for range time.Tick(time.Second) {
			if _, isBad := a.current(); isBad {
				a.logApp("ERROR [GC-thread] java.lang.OutOfMemoryError: Java heap space")
			}
		}
	}()
	if *rps > 0 {
		go func() {
			client := &http.Client{Timeout: 2 * time.Second}
			for range time.Tick(time.Second / time.Duration(*rps)) {
				go func() {
					resp, err := client.Get("http://" + *addr + "/api/orders?id=" + fmt.Sprint(rand.IntN(1000)))
					if err == nil {
						resp.Body.Close()
					}
				}()
			}
		}()
	}
	log.Printf("fakeapp %s listening on %s (bad versions: %s)", *version, *addr, *bad)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
