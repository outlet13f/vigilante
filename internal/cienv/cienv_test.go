package cienv

import (
	"errors"
	"testing"
)

func env(kv ...string) Getenv {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

func TestDeploymentID(t *testing.T) {
	cases := []struct {
		env  Getenv
		want string
	}{
		{env("VIGILANTE_DEPLOYMENT_ID", "rel-42", "BUILD_TAG", "jenkins-x-1"), "rel-42"},
		{env("BUILD_TAG", "jenkins-order-118"), "jenkins-order-118"},
		{env("CI_PIPELINE_ID", "9001"), "gl-9001"},
		{env("GITHUB_RUN_ID", "777", "GITHUB_RUN_ATTEMPT", "2"), "gh-777-2"},
		{env("GITHUB_RUN_ID", "777"), "gh-777-1"},
		{env(), ""},
	}
	for _, c := range cases {
		if got := DeploymentID(c.env); got.Value != c.want {
			t.Errorf("got %q (%s), want %q", got.Value, got.Source, c.want)
		}
	}
}

func TestVersion(t *testing.T) {
	git := func() (string, error) { return "v1.4.0-3-gabc1234", nil }
	noGit := func() (string, error) { return "", errors.New("not a repo") }
	cases := []struct {
		env  Getenv
		git  func() (string, error)
		want string
	}{
		{env("VIGILANTE_VERSION", "v9", "CI_COMMIT_TAG", "v1"), git, "v9"},
		{env("CI_COMMIT_TAG", "v1.5.0", "CI_COMMIT_SHORT_SHA", "abc12345"), git, "v1.5.0"},
		{env("GITHUB_REF_TYPE", "tag", "GITHUB_REF_NAME", "v2.0.0", "GITHUB_SHA", "0123456789abcdef"), git, "v2.0.0"},
		{env("GITHUB_REF_TYPE", "branch", "GITHUB_REF_NAME", "main", "GITHUB_SHA", "0123456789abcdef"), git, "01234567"},
		{env("GIT_COMMIT", "fedcba9876543210"), git, "fedcba98"},
		{env(), git, "v1.4.0-3-gabc1234"},
		{env(), noGit, ""},
	}
	for _, c := range cases {
		if got := Version(c.env, c.git); got.Value != c.want {
			t.Errorf("got %q (%s), want %q", got.Value, got.Source, c.want)
		}
	}
}
