// Package cienv fills deployment inputs that CI systems already know, so a
// pipeline step can be `vigilante watch --service S --phase canary` instead of
// passing the deployment ID and version by hand.
package cienv

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Value is a resolved input and where it came from (shown to the operator and
// recorded, so an auto-filled value is never a mystery).
type Value struct {
	Value  string
	Source string
}

func (v Value) Empty() bool { return v.Value == "" }

// Getenv matches os.Getenv; injectable for tests.
type Getenv func(string) string

// DeploymentID identifies the pipeline run, so prepare, watch and rollback
// steps of one run address the same deployment. Every stage of a GitLab
// pipeline shares CI_PIPELINE_ID; a GitHub re-run gets a new attempt number
// and therefore a new deployment.
func DeploymentID(getenv Getenv) Value {
	if v := getenv("VIGILANTE_DEPLOYMENT_ID"); v != "" {
		return Value{v, "VIGILANTE_DEPLOYMENT_ID"}
	}
	if v := getenv("BUILD_TAG"); v != "" { // Jenkins: jenkins-<job>-<build>
		return Value{v, "Jenkins BUILD_TAG"}
	}
	if v := getenv("CI_PIPELINE_ID"); v != "" {
		return Value{"gl-" + v, "GitLab CI_PIPELINE_ID"}
	}
	if v := getenv("GITHUB_RUN_ID"); v != "" {
		attempt := getenv("GITHUB_RUN_ATTEMPT")
		if attempt == "" {
			attempt = "1"
		}
		return Value{"gh-" + v + "-" + attempt, "GitHub GITHUB_RUN_ID"}
	}
	return Value{}
}

// Version names the release being deployed: an explicit override, then a
// release tag, then the commit, then `git describe` in the working directory.
func Version(getenv Getenv, gitDescribe func() (string, error)) Value {
	if v := getenv("VIGILANTE_VERSION"); v != "" {
		return Value{v, "VIGILANTE_VERSION"}
	}
	if v := getenv("CI_COMMIT_TAG"); v != "" {
		return Value{v, "GitLab CI_COMMIT_TAG"}
	}
	if getenv("GITHUB_REF_TYPE") == "tag" {
		if v := getenv("GITHUB_REF_NAME"); v != "" {
			return Value{v, "GitHub tag GITHUB_REF_NAME"}
		}
	}
	if v := getenv("CI_COMMIT_SHORT_SHA"); v != "" {
		return Value{v, "GitLab CI_COMMIT_SHORT_SHA"}
	}
	if v := getenv("GITHUB_SHA"); v != "" {
		return Value{short(v), "GitHub GITHUB_SHA"}
	}
	if v := getenv("GIT_COMMIT"); v != "" {
		return Value{short(v), "Jenkins GIT_COMMIT"}
	}
	if gitDescribe != nil {
		if v, err := gitDescribe(); err == nil && v != "" {
			return Value{v, "git describe"}
		}
	}
	return Value{}
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// GitDescribe runs `git describe --tags --always` in the current directory.
func GitDescribe() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "describe", "--tags", "--always").Output()
	return strings.TrimSpace(string(out)), err
}
