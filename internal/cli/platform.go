package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/Sigeth/opslab-core-go/api/v1alpha1"
	"github.com/Sigeth/opslab-core-go/pkg/project"

	"github.com/Sigeth/opslab-cli/internal/kube"
)

// defaultInstancesNamespace holds ChallengeInstances and CheckRuns.
const defaultInstancesNamespace = project.Name + "-instances"

var (
	challengeInstanceGVR = schema.GroupVersionResource{Group: v1alpha1.GroupVersion.Group, Version: v1alpha1.GroupVersion.Version, Resource: "challengeinstances"}
	checkRunGVR          = schema.GroupVersionResource{Group: v1alpha1.GroupVersion.Group, Version: v1alpha1.GroupVersion.Version, Resource: "checkruns"}
	challengeGVR         = schema.GroupVersionResource{Group: v1alpha1.GroupVersion.Group, Version: v1alpha1.GroupVersion.Version, Resource: "challenges"}
)

// errTimeout is returned when the platform took too long.
var errTimeout = errors.New("timed out")

// platform drives a running platform the way the api does: it installs
// challenges, starts instances and runs checks through the custom resources
// the operator watches (D2).
type platform struct {
	cluster               *kube.Cluster
	out                   io.Writer
	systemNS, instancesNS string
	timeout               time.Duration
}

// randomSuffix makes the names of one run unique: the namespace of a
// previous run may still be terminating.
func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// challenge returns an installed challenge, or nil.
func (p platform) challenge(ctx context.Context, name string) (*v1alpha1.Challenge, error) {
	obj, err := p.cluster.Dynamic.Resource(challengeGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, nil // not installed, or not readable yet
	}
	var ch v1alpha1.Challenge
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &ch); err != nil {
		return nil, err
	}
	return &ch, nil
}

// waitChallenge waits until a challenge is installed and runnable.
func (p platform) waitChallenge(ctx context.Context, name string) error {
	return p.poll(ctx, func() (bool, error) {
		ch, err := p.challenge(ctx, name)
		if err != nil || ch == nil {
			return false, err
		}
		for _, c := range ch.Status.Conditions {
			if c.Type == v1alpha1.ConditionReady && c.Status == metav1.ConditionFalse && c.ObservedGeneration == ch.Generation {
				return false, fmt.Errorf("the platform refused the challenge: %s", c.Message)
			}
		}
		if ch.Status.Package != nil && !ch.Status.Runnable {
			return false, fmt.Errorf("this cluster cannot run the challenge: %s", ch.Status.UnrunnableReason)
		}
		return ch.Status.Runnable, nil
	})
}

// startInstance creates an instance of challenge owned by the CLI.
func (p platform) startInstance(ctx context.Context, name, challenge, purpose string) error {
	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(), "kind": "ChallengeInstance",
		"metadata": map[string]any{"name": name, "namespace": p.instancesNS},
		"spec":     map[string]any{"challenge": challenge, "owner": map[string]any{"userID": project.Name + "-cli", "displayName": purpose}},
	}}
	return p.cluster.Apply(ctx, []*unstructured.Unstructured{instance})
}

// waitInstance waits until an instance is ready, printing its progress.
func (p platform) waitInstance(ctx context.Context, name string) (*v1alpha1.ChallengeInstance, error) {
	last := ""
	var ready *v1alpha1.ChallengeInstance
	err := p.poll(ctx, func() (bool, error) {
		obj, err := p.cluster.Dynamic.Resource(challengeInstanceGVR).Namespace(p.instancesNS).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		var inst v1alpha1.ChallengeInstance
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &inst); err != nil {
			return false, err
		}
		if msg := inst.Status.Message; msg != "" && msg != last {
			fmt.Fprintf(p.out, "  %s\n", msg)
			last = msg
		}
		switch inst.Status.Phase {
		case v1alpha1.InstanceReady:
			ready = &inst
			return true, nil
		case v1alpha1.InstanceFailed, v1alpha1.InstanceExpired:
			return false, fmt.Errorf("the instance %s: %s", inst.Status.Phase, inst.Status.Message)
		}
		return false, nil
	})
	return ready, err
}

// check runs the checks of an instance (run n) and returns the results.
func (p platform) check(ctx context.Context, instance string, n int) ([]v1alpha1.TaskResult, error) {
	name := fmt.Sprintf("%s-%d", instance, n)
	run := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(), "kind": "CheckRun",
		"metadata": map[string]any{"name": name, "namespace": p.instancesNS},
		"spec":     map[string]any{"instance": instance},
	}}
	if err := p.cluster.Apply(ctx, []*unstructured.Unstructured{run}); err != nil {
		return nil, err
	}
	var results []v1alpha1.TaskResult
	err := p.poll(ctx, func() (bool, error) {
		obj, err := p.cluster.Dynamic.Resource(checkRunGVR).Namespace(p.instancesNS).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		var run v1alpha1.CheckRun
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &run); err != nil {
			return false, err
		}
		switch run.Status.Phase {
		case v1alpha1.CheckRunCompleted:
			results = run.Status.Results
			return true, nil
		case v1alpha1.CheckRunError:
			return false, fmt.Errorf("the checks could not run: %s", run.Status.Message)
		}
		return false, nil
	})
	return results, err
}

// poll calls done every 3 seconds until it returns true or an error, or the
// timeout.
func (p platform) poll(ctx context.Context, done func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	for {
		ok, err := done()
		if ok || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w after %s", errTimeout, p.timeout)
		case <-time.After(3 * time.Second):
		}
	}
}

// deleteInstance removes an instance (best effort).
func (p platform) deleteInstance(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = p.cluster.Dynamic.Resource(challengeInstanceGVR).Namespace(p.instancesNS).Delete(ctx, name, metav1.DeleteOptions{})
}

// deleteChallenge removes a challenge and its files ConfigMap (best effort).
func (p platform) deleteChallenge(name, configMap string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = p.cluster.Dynamic.Resource(challengeGVR).Delete(ctx, name, metav1.DeleteOptions{})
	_ = p.cluster.Client.CoreV1().ConfigMaps(p.systemNS).Delete(ctx, configMap, metav1.DeleteOptions{})
}

func toUnstructured(objs ...runtime.Object) ([]*unstructured.Unstructured, error) {
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, obj := range objs {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return nil, err
		}
		u := &unstructured.Unstructured{Object: m}
		unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
		unstructured.RemoveNestedField(u.Object, "status")
		out = append(out, u)
	}
	return out, nil
}
