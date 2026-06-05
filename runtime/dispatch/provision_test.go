package dispatch

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestProvision_GitCredGrantCommandUsesConfiguredName(t *testing.T) {
	oldExec := execCommandContext
	defer func() { execCommandContext = oldExec }()

	var got []string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		got = append([]string{name}, args...)
		cmdArgs := []string{"-test.run=TestHelperProcess", "--"}
		cmdArgs = append(cmdArgs, got...)
		cmd := exec.CommandContext(ctx, os.Args[0], cmdArgs...)
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}

	c := &Controller{
		K8s: &K8s{
			Client:    fake.NewSimpleClientset(keyfileSecret("anvil")),
			Namespace: "nexus",
		},
		Cfg: JobConfig{
			Namespace:   "nexus",
			GitCredName: "github-dispatch",
		},
	}

	if err := c.Provision(context.Background(), Brief{
		Agent: "anvil",
		Repo:  "CarriedWorldUniverse/nexus",
		Task:  "do it",
	}, "task-1"); err != nil {
		t.Fatal(err)
	}

	want := []string{"cw", "credential", "issue-git-permission", "--aspect", "anvil", "--name", "github-dispatch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("grant command = %#v, want %#v", got, want)
	}
}

func TestProvision_SkipsGitCredGrantWhenNameEmpty(t *testing.T) {
	oldExec := execCommandContext
	defer func() { execCommandContext = oldExec }()

	called := false
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		called = true
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}

	c := &Controller{
		K8s: &K8s{
			Client:    fake.NewSimpleClientset(keyfileSecret("anvil")),
			Namespace: "nexus",
		},
		Cfg: JobConfig{Namespace: "nexus"},
	}

	if err := c.Provision(context.Background(), Brief{
		Agent: "anvil",
		Repo:  "CarriedWorldUniverse/nexus",
		Task:  "do it",
	}, "task-1"); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("git credential grant command ran with empty GitCredName")
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	os.Exit(0)
}
