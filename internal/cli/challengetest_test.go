package cli

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Sigeth/brokelabs-core-go/api/v1alpha1"
)

func testPkg(machines ...v1alpha1.Machine) *v1alpha1.ChallengePackage {
	return &v1alpha1.ChallengePackage{Spec: v1alpha1.ChallengePackageSpec{
		Machines: machines,
		Access:   v1alpha1.Access{Terminals: []v1alpha1.Terminal{{Machine: machines[0].Name}}},
	}}
}

func TestSolutionSteps(t *testing.T) {
	box := v1alpha1.Machine{Name: "box", Runtime: v1alpha1.RuntimePod}
	server := v1alpha1.Machine{Name: "server", Runtime: v1alpha1.RuntimeVM}

	steps, err := solutionSteps(testPkg(box), fstest.MapFS{"solution/solve.sh": {Data: []byte("x")}})
	if err != nil || len(steps) != 1 || steps[0].script != "solution/solve.sh" || steps[0].machine.Name != "box" {
		t.Fatalf("solve.sh: %+v, %v", steps, err)
	}

	// Per machine scripts win, in machine order; solve.sh only goes to the
	// first terminal machine.
	steps, err = solutionSteps(testPkg(box, server), fstest.MapFS{
		"solution/server.sh": {Data: []byte("s")},
		"solution/box.sh":    {Data: []byte("b")},
		"solution/solve.sh":  {Data: []byte("ignored")},
	})
	if err != nil || len(steps) != 2 || steps[0].script != "solution/box.sh" || steps[1].script != "solution/server.sh" {
		t.Fatalf("per machine: %+v, %v", steps, err)
	}

	if _, err := solutionSteps(testPkg(box), fstest.MapFS{}); err == nil || !strings.Contains(err.Error(), "no solution") {
		t.Fatalf("no solution: %v", err)
	}
	k := v1alpha1.Machine{Name: "k", Runtime: v1alpha1.RuntimeVCluster}
	if _, err := solutionSteps(testPkg(k), fstest.MapFS{"solution/k.sh": {Data: []byte("x")}}); err == nil {
		t.Fatal("vcluster machines cannot run scripts")
	}
}

func TestInstanceNameFitsANamespace(t *testing.T) {
	name := instanceName(strings.Repeat("a", 63))
	if len("inst-"+name) > 63 || !strings.Contains(name, "-test-") {
		t.Fatalf("%q is too long", name)
	}
	if a, b := instanceName("x"), instanceName("x"); a == b {
		t.Fatal("names must be unique")
	}
}

func TestReport(t *testing.T) {
	var out bytes.Buffer
	results := []v1alpha1.TaskResult{{Task: "a", Passed: false}, {Task: "b", Passed: true, Message: "already fine"}}
	if wrong := report(&out, results, func(string) bool { return false }); wrong != 1 {
		t.Fatalf("wrong = %d", wrong)
	}
	// A guard passes from the start.
	if wrong := report(&bytes.Buffer{}, results, func(task string) bool { return task == "b" }); wrong != 0 {
		t.Fatalf("with a guard: wrong = %d", wrong)
	}
	if !strings.Contains(out.String(), "FAIL b passes: already fine") {
		t.Fatalf("report = %q", out.String())
	}
}
