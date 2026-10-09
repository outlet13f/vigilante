package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"strings"
	"time"

	"vigilante/internal/auth"
)

// cliActor names the person running a local CLI command in deployment records.
func cliActor() string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	host, _ := os.Hostname()
	return "cli:" + name + "@" + host
}

// cmdToken: `vigilante token create --name ci-order [--role deployer] [--scope service=order-api] [--expires 2027-06-30]`
// prints a new service-account token once, plus the config entry holding its hash.
func cmdToken(args []string) (int, error) {
	if len(args) == 0 || args[0] != "create" {
		return 1, errors.New("usage: vigilante token create --name NAME [--role ROLE] [--scope SCOPE] [--expires YYYY-MM-DD]")
	}
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	name := fs.String("name", "", "service account name (e.g. ci-order-api)")
	role := fs.String("role", "deployer", "viewer | deployer | operator | admin | agent")
	scope := fs.String("scope", "*", "* | team=<team> | service=<name>")
	expires := fs.String("expires", "", "last valid day, YYYY-MM-DD (recommended)")
	if err := fs.Parse(args[1:]); err != nil {
		return 1, err
	}
	if *name == "" {
		return 1, errors.New("token create: --name is required")
	}
	if _, err := auth.ParseRole(*role); err != nil {
		return 1, err
	}
	if _, err := auth.ParseScope(*scope); err != nil {
		return 1, err
	}
	if *expires != "" {
		if _, err := time.Parse("2006-01-02", *expires); err != nil {
			return 1, fmt.Errorf("--expires: %w", err)
		}
	}
	token, sha, err := auth.NewToken()
	if err != nil {
		return 1, err
	}
	fmt.Fprintln(os.Stderr, "Token (shown once; store it as a CI secret, e.g. VIGILANTE_TOKEN):")
	fmt.Println(token)
	fmt.Fprintln(os.Stderr, "\nAdd to vigilante.yaml under auth.service_accounts (only the hash is stored):")
	entry := fmt.Sprintf("  - name: %s\n    token_sha256: %s\n", *name, sha)
	if *expires != "" {
		entry += fmt.Sprintf("    expires: %s\n", *expires)
	}
	entry += fmt.Sprintf("    roles: [{role: %s, scope: %q}]\n", *role, *scope)
	fmt.Fprint(os.Stderr, entry)
	return 0, nil
}

// cmdWhoami asks a server who the current VIGILANTE_TOKEN belongs to.
func cmdWhoami(ctx context.Context, args []string) (int, error) {
	c := newFlags("whoami")
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	if c.srv == "" {
		return 1, errors.New("whoami: --server is required")
	}
	var out struct {
		ID     string   `json:"id"`
		Kind   string   `json:"kind"`
		Groups []string `json:"groups"`
		Grants []string `json:"grants"`
	}
	if err := apiCall(ctx, c.srv, http.MethodGet, "/v1/whoami", nil, &out); err != nil {
		return 1, err
	}
	fmt.Printf("%s (%s)\n", out.ID, out.Kind)
	if len(out.Groups) > 0 {
		fmt.Printf("groups: %s\n", strings.Join(out.Groups, ", "))
	}
	if len(out.Grants) == 0 {
		fmt.Println("grants: none — this identity can authenticate but not act")
	} else {
		fmt.Printf("grants: %s\n", strings.Join(out.Grants, ", "))
	}
	return 0, nil
}
