package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/Sigeth/opslab-core-go/api/v1alpha1"
	"github.com/Sigeth/opslab-core-go/pkg/kubeexec"
	"github.com/Sigeth/opslab-core-go/pkg/project"
	"github.com/Sigeth/opslab-core-go/pkg/sshexec"

	"github.com/Sigeth/opslab-cli/internal/kube"
)

// SolveScript is the solution run on the first terminal machine when the
// machine has no script of its own (solution/<machine>.sh).
const SolveScript = SolutionDir + "/solve.sh"

var errChallengeTest = errors.New("the challenge test failed")

func newChallengeTestCommand() *cobra.Command {
	var (
		kubeconfig, kubeContext, systemNS, instancesNS string
		timeout                                        time.Duration
		keep, replace                                  bool
	)
	cmd := &cobra.Command{
		Use:   "test [DIR]",
		Short: "Check on a cluster that every task fails, then passes once solved",
		Long: "Installs the challenge in DIR (inline) on the platform of the current context, starts an\n" +
			"instance and runs its checks, which must all fail. It then runs the reference solution as the\n" +
			"student, the way a student would in a terminal, and runs the checks again: they must all pass.\n\n" +
			"The solution is " + SolutionDir + "/<machine>.sh for each machine that has one, or " + SolveScript + "\n" +
			"on the first terminal machine. Scripts run with sh, as the student (who has sudo on vm machines).\n" +
			"Machine images must be pullable by the cluster.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			c, err := kube.Connect(kubeconfig, kubeContext)
			if err != nil {
				return err
			}
			t := challengeTest{
				platform: platform{cluster: c, out: cmd.OutOrStdout(), systemNS: systemNS, instancesNS: instancesNS, timeout: timeout},
				dir:      dir,
				ssh:      sshexec.New(),
			}
			if t.exec, err = kubeexec.New(c.Config); err != nil {
				return err
			}
			return t.run(cmd.Context(), keep, replace)
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	cmd.Flags().StringVar(&kubeContext, "context", "", "kubeconfig context (default: the current one)")
	cmd.Flags().StringVar(&systemNS, "system-namespace", defaultSystemNamespace, "namespace of the opslab operator")
	cmd.Flags().StringVar(&instancesNS, "instances-namespace", defaultInstancesNamespace, "namespace of the instances")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "how long to wait for each step")
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the instance and the challenge (to investigate)")
	cmd.Flags().BoolVar(&replace, "replace", false, "test over an installed challenge of the same name (it stays installed, from DIR)")
	return cmd
}

type challengeTest struct {
	platform
	dir  string
	exec kubeexec.Executor
	ssh  sshexec.Executor
}

// solutionStep is a script to run on a machine.
type solutionStep struct {
	machine v1alpha1.Machine
	script  string
	data    []byte
}

func (t challengeTest) run(ctx context.Context, keep, replace bool) error {
	files := os.DirFS(t.dir)
	cm, ch, err := inlineChallenge(files, t.dir, t.systemNS)
	if err != nil {
		return err
	}
	pkg := &ch.Spec.Source.Inline.Package
	steps, err := solutionSteps(pkg, files)
	if err != nil {
		return err
	}
	name := pkg.Metadata.Name

	existing, err := t.challenge(ctx, name)
	if err != nil {
		return err
	}
	if existing != nil && !replace {
		return fmt.Errorf("a challenge named %s is installed: use --replace to test over it (it then stays installed, from %s)", name, t.dir)
	}
	objs, err := toUnstructured(cm, ch)
	if err != nil {
		return err
	}
	if err := t.cluster.Apply(ctx, objs); err != nil {
		return err
	}
	instance := instanceName(name)
	if !keep {
		if existing == nil {
			defer t.deleteChallenge(name, cm.Name)
		}
		defer t.deleteInstance(instance)
	}

	fmt.Fprintf(t.out, "installing %s %s\n", name, pkg.Metadata.Version)
	if err := t.waitChallenge(ctx, name); err != nil {
		return fmt.Errorf("%w: %v", errChallengeTest, err)
	}
	if err := t.startInstance(ctx, instance, name, "challenge test"); err != nil {
		return err
	}
	start := time.Now()
	inst, err := t.waitInstance(ctx, instance)
	if err != nil {
		return fmt.Errorf("%w: %v", errChallengeTest, err)
	}
	fmt.Fprintf(t.out, "instance ready after %s\n", time.Since(start).Round(time.Second))

	guards := map[string]bool{}
	for _, task := range pkg.Spec.Tasks {
		guards[task.ID] = task.Guard
	}
	fmt.Fprintln(t.out, "checks before the solution (all must fail, guards pass):")
	before, err := t.check(ctx, instance, 1)
	if err != nil {
		return fmt.Errorf("%w: %v", errChallengeTest, err)
	}
	unexpectedBefore := report(t.out, before, func(task string) bool { return guards[task] })

	if err := t.openSSH(ctx, inst.Status.Namespace, steps); err != nil {
		return err
	}
	for _, s := range steps {
		fmt.Fprintf(t.out, "running %s on %s\n", s.script, s.machine.Name)
		if err := t.solve(ctx, inst.Status.Namespace, s); err != nil {
			return fmt.Errorf("%w: %s on %s: %v", errChallengeTest, s.script, s.machine.Name, err)
		}
	}

	fmt.Fprintln(t.out, "checks after the solution (all must pass):")
	after, err := t.check(ctx, instance, 2)
	if err != nil {
		return fmt.Errorf("%w: %v", errChallengeTest, err)
	}
	failing := report(t.out, after, func(string) bool { return true })

	var problems []string
	if unexpectedBefore > 0 {
		problems = append(problems, fmt.Sprintf("%d task(s) are already solved or break before the solution (mark tasks true from the start as guard: true)", unexpectedBefore))
	}
	if failing > 0 {
		problems = append(problems, fmt.Sprintf("%d task(s) still fail with the solution", failing))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", errChallengeTest, strings.Join(problems, ", "))
	}
	fmt.Fprintf(t.out, "%s: every task fails before the solution and passes after it, guards hold\n", name)
	return nil
}

// report prints check results against what is expected of each task
// (passing or not) and returns how many tasks did not meet it.
func report(out io.Writer, results []v1alpha1.TaskResult, wantPassed func(task string) bool) int {
	wrong := 0
	for _, r := range results {
		mark := "ok  "
		if r.Passed != wantPassed(r.Task) {
			mark, wrong = "FAIL", wrong+1
		}
		state := "fails"
		if r.Passed {
			state = "passes"
		}
		fmt.Fprintf(out, "  %s %s %s: %s\n", mark, r.Task, state, r.Message)
	}
	return wrong
}

// solutionSteps lists the scripts of the solution, in machine order:
// solution/<machine>.sh, or solution/solve.sh on the first terminal machine.
func solutionSteps(pkg *v1alpha1.ChallengePackage, files fs.FS) ([]solutionStep, error) {
	var steps []solutionStep
	usedSolve := false
	for _, m := range pkg.Spec.Machines {
		script := SolutionDir + "/" + m.Name + ".sh"
		data, err := fs.ReadFile(files, script)
		if errors.Is(err, fs.ErrNotExist) && !usedSolve && len(pkg.Spec.Access.Terminals) > 0 && pkg.Spec.Access.Terminals[0].Machine == m.Name {
			script = SolveScript
			data, err = fs.ReadFile(files, script)
			usedSolve = err == nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if m.Runtime == v1alpha1.RuntimeVCluster {
			return nil, fmt.Errorf("%s: vcluster machines have no shell to run a solution on", script)
		}
		steps = append(steps, solutionStep{machine: m, script: script, data: data})
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("no solution: write %s or %s/<machine>.sh", SolveScript, SolutionDir)
	}
	return steps, nil
}

// sshTestPolicy lets SSH into the vm machines of a test instance from
// anywhere: KubeVirt port forwarding connects from the node, which the
// default-deny policy of instances refuses. Test instances only; the policy
// goes with the namespace.
const sshTestPolicy = project.Name + "-challenge-test-ssh"

// openSSH admits port forwarding to the vm machines that run a solution.
func (t challengeTest) openSSH(ctx context.Context, namespace string, steps []solutionStep) error {
	var vms []string
	for _, s := range steps {
		if s.machine.Runtime == v1alpha1.RuntimeVM {
			vms = append(vms, s.machine.Name)
		}
	}
	if len(vms) == 0 {
		return nil
	}
	port := intstr.FromInt32(22)
	tcp := corev1.ProtocolTCP
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: sshTestPolicy, Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: project.LabelPrefix + "machine", Operator: metav1.LabelSelectorOpIn, Values: vms},
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}}}},
		},
	}
	_, err := t.cluster.Client.NetworkingV1().NetworkPolicies(namespace).Create(ctx, policy, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("letting the test into the vm machines over SSH: %w", err)
	}
	return nil
}

// dialVM opens SSH port forwarding to a vm machine. The network policy
// admitting it takes a few seconds to apply, so refusals are retried.
func (t challengeTest) dialVM(ctx context.Context, namespace, vm string) (net.Conn, error) {
	for {
		conn, err := t.cluster.VMPortForward(ctx, namespace, vm, 22)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(2 * time.Second):
		}
	}
}

// solve runs a solution script as the student on its machine: through exec
// on pod machines (whose user is the student), over SSH on vm machines,
// tunnelled through the API server (KubeVirt port forwarding).
func (t challengeTest) solve(ctx context.Context, namespace string, s solutionStep) error {
	var stdout, stderr []byte
	var exit int
	switch s.machine.Runtime {
	case v1alpha1.RuntimeVM:
		secret, err := t.cluster.Client.CoreV1().Secrets(namespace).Get(ctx, v1alpha1.SSHSecret, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading the SSH keys of the instance: %w", err)
		}
		res, err := t.ssh.Run(ctx, sshexec.Request{
			Addr:       s.machine.Name + "." + namespace + ".svc:22",
			User:       v1alpha1.StudentUser,
			PrivateKey: secret.Data[v1alpha1.SSHSecretStudentKey],
			KnownHosts: secret.Data[v1alpha1.SSHSecretKnownHosts],
			Command:    "sh -s",
			Stdin:      bytes.NewReader(s.data),
			Dial: func(ctx context.Context) (net.Conn, error) {
				return t.dialVM(ctx, namespace, s.machine.Name)
			},
			ConnectTimeout: 45 * time.Second,
		})
		if err != nil {
			return err
		}
		stdout, stderr, exit = res.Stdout, res.Stderr, res.ExitCode
	default:
		res, err := t.exec.Exec(ctx, kubeexec.Request{
			Namespace: namespace,
			Pod:       s.machine.Name,
			Container: v1alpha1.MachineContainer,
			Command:   []string{"sh", "-s"},
			Stdin:     bytes.NewReader(s.data),
		})
		if err != nil {
			return err
		}
		stdout, stderr, exit = res.Stdout, res.Stderr, res.ExitCode
	}
	for _, line := range strings.Split(strings.TrimRight(string(stdout)+string(stderr), "\n"), "\n") {
		if line != "" {
			fmt.Fprintf(t.out, "  | %s\n", line)
		}
	}
	if exit != 0 {
		return fmt.Errorf("exited with code %d", exit)
	}
	return nil
}

// instanceName names the instance of a test run: unique, and short enough
// for its namespace name (inst-<name>, at most 63 characters).
func instanceName(challenge string) string {
	const max = 63 - len("inst-") - len("-test-") - 8
	if len(challenge) > max {
		challenge = strings.TrimRight(challenge[:max], "-")
	}
	return challenge + "-test-" + randomSuffix()
}
