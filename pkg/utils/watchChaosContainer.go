package utils

import (
	"context"
	"github.com/litmuschaos/chaos-runner/pkg/log"
	"time"

	"github.com/litmuschaos/litmus-go/pkg/utils/retry"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// GetChaosPod gets the chaos experiment pod object launched by the runner
func GetChaosPod(expDetails *ExperimentDetails, clients ClientSets) (*corev1.Pod, error) {
	var chaosPodList *corev1.PodList
	var err error

	delay := 2
	err = retry.
		Times(uint(expDetails.StatusCheckTimeout / delay)).
		Wait(time.Duration(delay) * time.Second).
		Try(func(attempt uint) error {
			chaosPodList, err = clients.KubeClient.CoreV1().Pods(expDetails.Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "job-name=" + expDetails.JobName})
			if err != nil || len(chaosPodList.Items) == 0 {
				return errors.Errorf("unable to get the chaos pod, error: %v", err)
			} else if len(chaosPodList.Items) > 1 {
				// Cases where experiment pod is rescheduled by the job controller due to
				// issues while the older pod is still not cleaned-up
				return errors.Errorf("Multiple pods exist with same job-name label")
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	// Note: We error out upon existence of multiple exp pods for the same experiment
	// & hence use index [0]
	chaosPod := &chaosPodList.Items[0]
	return chaosPod, nil
}

// GetChaosContainerStatus gets status of the chaos container
func GetChaosContainerStatus(experimentDetails *ExperimentDetails, pod *corev1.Pod, startTime time.Time) (bool, error) {

	isCompleted := false

	if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodSucceeded {
		for _, container := range pod.Status.ContainerStatuses {

			//NOTE: The name of container inside chaos-pod is same as the chaos job name
			// we only have one container inside chaos pod to inject the chaos
			// looking the chaos container is completed or not
			if container.Name == experimentDetails.JobName && container.State.Terminated != nil {
				if container.State.Terminated.Reason == "Completed" {
					isCompleted = !container.Ready
				}
			}
		}

	} else if pod.Status.Phase == corev1.PodPending {
		currentTime := time.Duration(experimentDetails.StatusCheckTimeout) * time.Second
		if time.Now().Sub(startTime) > currentTime {
			return isCompleted, errors.Errorf("chaos pod is in %v state", corev1.PodPending)
		}
		return false, nil
	} else if pod.Status.Phase == corev1.PodFailed {
		return isCompleted, errors.Errorf("status check failed as chaos pod status is %v", pod.Status.Phase)
	}

	return isCompleted, nil
}

// WatchChaosContainerForCompletion watches the chaos container for completion
func (engineDetails EngineDetails) WatchChaosContainerForCompletion(experiment *ExperimentDetails, clients ClientSets) error {

	expPod, err := clients.KubeClient.CoreV1().Pods(experiment.Namespace).Watch(context.Background(), metav1.ListOptions{LabelSelector: "job-name=" + experiment.JobName})
	if err != nil {
		return err
	}

	defer expPod.Stop()

	startTime := time.Now()

loop:
	for {
		select {
		case event := <-expPod.ResultChan():
			pod := event.Object.(*corev1.Pod)

			if event.Type == watch.Added {
				var expStatus ExperimentStatus
				expStatus.AwaitedExperimentStatus(experiment.Name, engineDetails.Name, pod.Name)
				if err := expStatus.PatchChaosEngineStatus(engineDetails, clients); err != nil {
					return errors.Errorf("unable to patch ChaosEngine in namespace: %v, error: %v", engineDetails.EngineNamespace, err)
				}
			}

			if event.Type == watch.Deleted {
				log.Info("experiment pod deleted")
				continue
				//expPod.Stop()
				//return errors.Errorf("experiment pod is deleted unexpectedly")
			}

			completed, err := GetChaosContainerStatus(experiment, pod, startTime)
			if err != nil {
				return err
			}
			if completed {
				break loop
			}
		}
	}
	return nil
}
