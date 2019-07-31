// Package drainer is mostly transplanted from kubectl
// See: https://github.com/kubernetes/kubernetes/blob/master/pkg/kubectl/cmd/drain/drain.go
package drainer

import (
	"fmt"
	"github.com/VirtusLab/kubedrainer/cmd/pkg/kubernetes/node"
	"math"
	"time"

	"github.com/golang/glog"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kubectl/pkg/drain"
)

type Drainer interface {
	Drain(nodeName string) error
}

type Options struct {
	Force               bool
	DryRun              bool
	GracePeriodSeconds  int
	IgnoreAllDaemonSets bool
	Timeout             time.Duration
	DeleteLocalData     bool
	Selector            string
	PodSelector         string
}

// ErrWriter allows for Glog usage inside kubectl drainer implementation
type ErrWriter struct{}

// OutWriter allows for Glog usage inside kubectl drainer implementation
type OutWriter struct{}

func (ErrWriter) Write(p []byte) (n int, err error) {
	glog.Error(string(p))
	return len(p), nil
}

func (OutWriter) Write(p []byte) (n int, err error) {
	glog.Info(string(p))
	return len(p), nil
}

func New(client kubernetes.Interface, options *Options) Drainer {
	out := &OutWriter{}
	errOut := &ErrWriter{}
	return &drainCmdOptions{
		PrintFlags: genericclioptions.NewPrintFlags("drained").WithTypeSetter(scheme.Scheme),
		ToPrinter:  nil, // must be initialized before use
		IOStreams: genericclioptions.IOStreams{
			ErrOut: errOut,
			Out:    out,
		},
		drainer: &drain.Helper{
			Client:              client,
			ErrOut:              errOut,
			DryRun:              options.DryRun,
			Force:               options.Force,
			GracePeriodSeconds:  options.GracePeriodSeconds,
			IgnoreAllDaemonSets: options.IgnoreAllDaemonSets,
			Timeout:             options.Timeout,
			DeleteLocalData:     options.DeleteLocalData,
			Selector:            options.Selector,
			PodSelector:         options.PodSelector,
		},
	}
}

func (o *drainCmdOptions) Drain(nodeName string) error {
	glog.Infof("Draining node: '%s'", nodeName)
	n := node.Node{
		Client: o.drainer.Client,
	}
	if _, err := n.GetNode(nodeName); err != nil {
		return err
	}

	o.ToPrinter = func(operation string) (printers.ResourcePrinterFunc, error) {
		o.PrintFlags.NamePrintFlags.Operation = operation
		if o.drainer.DryRun {
			err := o.PrintFlags.Complete("%s (dry run)")
			if err != nil {
				return nil, err
			}
		}

		printer, err := o.PrintFlags.ToPrinter()
		if err != nil {
			return nil, err
		}

		return printer.PrintObj, nil
	}

	return o.deleteOrEvictPodsSimple(nodeName)
}

// ---- kubectl stuff ---------------------------------------------------------

type drainCmdOptions struct {
	PrintFlags *genericclioptions.PrintFlags
	ToPrinter  func(string) (printers.ResourcePrinterFunc, error)

	drainer *drain.Helper

	genericclioptions.IOStreams
}

func (o *drainCmdOptions) deleteOrEvictPodsSimple(nodeName string) error {
	list, errs := o.drainer.GetPodsForDeletion(nodeName)
	if errs != nil {
		return utilerrors.NewAggregate(errs)
	}
	if warnings := list.Warnings(); warnings != "" {
		_, _ = fmt.Fprintf(o.ErrOut, "WARNING: %s\n", warnings)
	}

	if err := o.deleteOrEvictPods(list.Pods()); err != nil {
		pendingList, newErrs := o.drainer.GetPodsForDeletion(nodeName)

		_, _ = fmt.Fprintf(o.ErrOut, "There are pending pods in node %q when an error occurred: %v\n", nodeName, err)
		for _, pendingPod := range pendingList.Pods() {
			_, _ = fmt.Fprintf(o.ErrOut, "%s/%s\n", "pod", pendingPod.Name)
		}
		if newErrs != nil {
			_, _ = fmt.Fprintf(o.ErrOut, "following errors also occurred:\n%s", utilerrors.NewAggregate(newErrs))
		}
		return err
	}
	return nil
}

// deleteOrEvictPods deletes or evicts the pods on the api server
func (o *drainCmdOptions) deleteOrEvictPods(pods []corev1.Pod) error {
	if len(pods) == 0 {
		return nil
	}

	policyGroupVersion, err := drain.CheckEvictionSupport(o.drainer.Client)
	if err != nil {
		return err
	}

	getPodFn := func(namespace, name string) (*corev1.Pod, error) {
		return o.drainer.Client.CoreV1().Pods(namespace).Get(name, metav1.GetOptions{})
	}

	if len(policyGroupVersion) > 0 {
		return o.evictPods(pods, policyGroupVersion, getPodFn)
	} else {
		return o.deletePods(pods, getPodFn)
	}
}

func (o *drainCmdOptions) evictPods(pods []corev1.Pod, policyGroupVersion string, getPodFn func(namespace, name string) (*corev1.Pod, error)) error {
	returnCh := make(chan error, 1)

	for _, pod := range pods {
		go func(pod corev1.Pod, returnCh chan error) {
			for {
				_, _ = fmt.Fprintf(o.Out, "evicting pod %q\n", pod.Name)
				err := o.drainer.EvictPod(pod, policyGroupVersion)
				if err == nil {
					break
				} else if apierrors.IsNotFound(err) {
					returnCh <- nil
					return
				} else if apierrors.IsTooManyRequests(err) {
					_, _ = fmt.Fprintf(o.ErrOut, "error when evicting pod %q (will retry after 5s): %v\n", pod.Name, err)
					time.Sleep(5 * time.Second)
				} else {
					returnCh <- fmt.Errorf("error when evicting pod %q: %v", pod.Name, err)
					return
				}
			}
			_, err := o.waitForDelete([]corev1.Pod{pod}, 1*time.Second, time.Duration(math.MaxInt64), true, getPodFn)
			if err == nil {
				returnCh <- nil
			} else {
				returnCh <- fmt.Errorf("error when waiting for pod %q terminating: %v", pod.Name, err)
			}
		}(pod, returnCh)
	}

	doneCount := 0
	var errs []error

	// 0 timeout means infinite, we use MaxInt64 to represent it.
	var globalTimeout time.Duration
	if o.drainer.Timeout == 0 {
		globalTimeout = time.Duration(math.MaxInt64)
	} else {
		globalTimeout = o.drainer.Timeout
	}
	globalTimeoutCh := time.After(globalTimeout)
	numPods := len(pods)
	for doneCount < numPods {
		select {
		case err := <-returnCh:
			doneCount++
			if err != nil {
				errs = append(errs, err)
			}
		case <-globalTimeoutCh:
			return fmt.Errorf("drain did not complete within %v", globalTimeout)
		}
	}
	return utilerrors.NewAggregate(errs)
}

func (o *drainCmdOptions) deletePods(pods []corev1.Pod, getPodFn func(namespace, name string) (*corev1.Pod, error)) error {
	// 0 timeout means infinite, we use MaxInt64 to represent it.
	var globalTimeout time.Duration
	if o.drainer.Timeout == 0 {
		globalTimeout = time.Duration(math.MaxInt64)
	} else {
		globalTimeout = o.drainer.Timeout
	}
	for _, pod := range pods {
		err := o.drainer.DeletePod(pod)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	_, err := o.waitForDelete(pods, 1*time.Second, globalTimeout, false, getPodFn)
	return err
}

func (o *drainCmdOptions) waitForDelete(pods []corev1.Pod, interval, timeout time.Duration, usingEviction bool, getPodFn func(string, string) (*corev1.Pod, error)) ([]corev1.Pod, error) {
	var verbStr string
	if usingEviction {
		verbStr = "evicted"
	} else {
		verbStr = "deleted"
	}
	printObj, err := o.ToPrinter(verbStr)
	if err != nil {
		return pods, err
	}

	err = wait.PollImmediate(interval, timeout, func() (bool, error) {
		var pendingPods []corev1.Pod
		for i, pod := range pods {
			p, err := getPodFn(pod.Namespace, pod.Name)
			if apierrors.IsNotFound(err) || (p != nil && p.ObjectMeta.UID != pod.ObjectMeta.UID) {
				_ = printObj(&pod, o.Out)
				continue
			} else if err != nil {
				return false, err
			} else {
				pendingPods = append(pendingPods, pods[i])
			}
		}
		pods = pendingPods
		if len(pendingPods) > 0 {
			return false, nil
		}
		return true, nil
	})
	return pods, err
}
