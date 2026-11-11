/*
Copyright 2026 The RBG Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package runtime_label

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/rbgs/api/workloads/constants"
	workloadsv1alpha2 "sigs.k8s.io/rbgs/api/workloads/v1alpha2"
	"sigs.k8s.io/rbgs/test/envtest/testutil"
	wrappersv2 "sigs.k8s.io/rbgs/test/wrappers/v1alpha2"
)

const (
	timeout    = time.Second * 60
	interval   = time.Millisecond * 250
	soakWindow = time.Second * 5

	roleName = "pdmix-pool"

	// A runtime serving-role label owned by an external switch service, not by RBG.
	// The key is deliberately outside every RBG-managed prefix.
	runtimeRoleLabelKey  = "inference.workload.x-k8s.io/pd-role"
	runtimeAnnotationKey = "inference.workload.x-k8s.io/pd-state"

	// A key RBG does own. Rolling it alongside the image forces the in-place
	// update to carry a metadata patch instead of an image-only diff.
	templateOwnedAnnotationKey = "test.rbg/template-owned"

	updatedImage = "registry.cn-hangzhou.aliyuncs.com/acs-sample/nginx:1.25"
)

var _ = Describe("Runtime label ownership contract", func() {
	var testNs string

	BeforeEach(func() {
		testNs = fmt.Sprintf("test-runtime-label-%d", time.Now().UnixNano())
		testutil.CreateNamespace(testNs)
	})

	AfterEach(func() {
		testutil.DeleteNamespace(testNs)
	})

	// newRBG builds a single-role group whose pod template carries the initial
	// runtime label value. The template value is only a starting point: the
	// external switch service owns the live value from then on.
	newRBG := func(rbgName, initial string) *workloadsv1alpha2.RoleBasedGroup {
		template := wrappersv2.BuildBasicPodTemplateSpec()
		template.Labels = map[string]string{runtimeRoleLabelKey: initial}
		template.Annotations = map[string]string{
			runtimeAnnotationKey:       initial,
			templateOwnedAnnotationKey: "v1",
		}

		role := wrappersv2.BuildStandaloneRole(roleName).
			WithReplicas(1).
			WithTemplate(&template).
			Obj()

		return wrappersv2.BuildBasicRoleBasedGroup(rbgName, testNs).
			WithRoles([]workloadsv1alpha2.RoleSpec{role}).
			Obj()
	}

	waitForPod := func(rbgName string) *corev1.Pod {
		podKey := types.NamespacedName{}
		Eventually(func() error {
			podList := &corev1.PodList{}
			if err := testutil.K8sClient.List(testutil.Ctx, podList,
				client.InNamespace(testNs),
				client.MatchingLabels{
					constants.GroupNameLabelKey: rbgName,
					constants.RoleNameLabelKey:  roleName,
				}); err != nil {
				return err
			}
			if len(podList.Items) != 1 {
				return fmt.Errorf("expected 1 pod for role %s, got %d", roleName, len(podList.Items))
			}
			podKey = client.ObjectKeyFromObject(&podList.Items[0])
			return nil
		}, timeout, interval).Should(Succeed())

		pod := &corev1.Pod{}
		Expect(testutil.K8sClient.Get(testutil.Ctx, podKey, pod)).Should(Succeed())
		return pod
	}

	// patchFromOutside mimics an external switch service: a minimal merge patch
	// touching one key, with no knowledge of the rest of the object.
	patchFromOutside := func(pod *corev1.Pod, field, key, value string) {
		patch := client.RawPatch(types.MergePatchType,
			[]byte(fmt.Sprintf(`{"metadata":{"%s":{%q:%q}}}`, field, key, value)))
		Expect(testutil.K8sClient.Patch(testutil.Ctx, pod, patch)).Should(Succeed())
	}

	// forceReconcile nudges the group so the controller runs another pass while
	// the externally written value is on the pod.
	forceReconcile := func(rbgName string) {
		rbg := &workloadsv1alpha2.RoleBasedGroup{}
		Expect(testutil.K8sClient.Get(testutil.Ctx,
			types.NamespacedName{Name: rbgName, Namespace: testNs}, rbg)).Should(Succeed())
		patch := client.RawPatch(types.MergePatchType,
			[]byte(fmt.Sprintf(`{"metadata":{"annotations":{"test.rbg/nudge":"%d"}}}`, time.Now().UnixNano())))
		Expect(testutil.K8sClient.Patch(testutil.Ctx, rbg, patch)).Should(Succeed())
	}

	readMeta := func(podKey types.NamespacedName, field, key string) func() (string, error) {
		return func() (string, error) {
			pod := &corev1.Pod{}
			if err := testutil.K8sClient.Get(testutil.Ctx, podKey, pod); err != nil {
				return "", err
			}
			if field == "labels" {
				return pod.Labels[key], nil
			}
			return pod.Annotations[key], nil
		}
	}

	readPod := func(podKey types.NamespacedName) *corev1.Pod {
		pod := &corev1.Pod{}
		Expect(testutil.K8sClient.Get(testutil.Ctx, podKey, pod)).Should(Succeed())
		return pod
	}

	// fakeKubelet stands in for the node. envtest runs no kubelet, so container
	// status has to be written by hand — including the ImageID the controller
	// compares against to decide an in-place update has finished.
	fakeKubelet := func(podKey types.NamespacedName, imageID string) {
		Eventually(func() error {
			pod := &corev1.Pod{}
			if err := testutil.K8sClient.Get(testutil.Ctx, podKey, pod); err != nil {
				return err
			}
			testutil.SetPodRunningAndReady(pod)
			for i := range pod.Status.ContainerStatuses {
				pod.Status.ContainerStatuses[i].ImageID = imageID
				pod.Status.ContainerStatuses[i].ContainerID = "containerd://" + imageID
			}
			return testutil.K8sClient.Status().Update(testutil.Ctx, pod)
		}, timeout, interval).Should(Succeed())
	}

	rollTemplate := func(rbgName, image, ownedValue string) {
		Eventually(func() error {
			rbg := &workloadsv1alpha2.RoleBasedGroup{}
			if err := testutil.K8sClient.Get(testutil.Ctx,
				types.NamespacedName{Name: rbgName, Namespace: testNs}, rbg); err != nil {
				return err
			}
			template := rbg.Spec.Roles[0].StandalonePattern.Template
			template.Spec.Containers[0].Image = image
			template.Annotations[templateOwnedAnnotationKey] = ownedValue
			return testutil.K8sClient.Update(testutil.Ctx, rbg)
		}, timeout, interval).Should(Succeed())
	}

	inPlaceCondition := func(podKey types.NamespacedName) func() corev1.ConditionStatus {
		return func() corev1.ConditionStatus {
			for _, c := range readPod(podKey).Status.Conditions {
				if c.Type == constants.InPlaceUpdateReady {
					return c.Status
				}
			}
			return ""
		}
	}

	It("Should keep an externally patched pod label across reconciliation", func() {
		rbgName := "pd-switch-label"
		Expect(testutil.K8sClient.Create(testutil.Ctx, newRBG(rbgName, "unassigned"))).Should(Succeed())

		pod := waitForPod(rbgName)
		podKey := client.ObjectKeyFromObject(pod)
		Expect(pod.Labels[runtimeRoleLabelKey]).To(Equal("unassigned"))

		patchFromOutside(pod, "labels", runtimeRoleLabelKey, "prefill")
		forceReconcile(rbgName)

		// A single read could pass by luck before the controller reacts; the
		// contract is that no later reconcile reverts the value either.
		Consistently(readMeta(podKey, "labels", runtimeRoleLabelKey),
			soakWindow, interval).Should(Equal("prefill"))

		// Preserving the value by recreating the pod would be a false pass.
		fresh := &corev1.Pod{}
		Expect(testutil.K8sClient.Get(testutil.Ctx, podKey, fresh)).Should(Succeed())
		Expect(fresh.UID).To(Equal(pod.UID))
	})

	It("Should keep an externally patched pod annotation across reconciliation", func() {
		rbgName := "pd-switch-annotation"
		Expect(testutil.K8sClient.Create(testutil.Ctx, newRBG(rbgName, "unassigned"))).Should(Succeed())

		pod := waitForPod(rbgName)
		podKey := client.ObjectKeyFromObject(pod)
		Expect(pod.Annotations[runtimeAnnotationKey]).To(Equal("unassigned"))

		patchFromOutside(pod, "annotations", runtimeAnnotationKey, "draining")
		forceReconcile(rbgName)

		Consistently(readMeta(podKey, "annotations", runtimeAnnotationKey),
			soakWindow, interval).Should(Equal("draining"))

		fresh := &corev1.Pod{}
		Expect(testutil.K8sClient.Get(testutil.Ctx, podKey, fresh)).Should(Succeed())
		Expect(fresh.UID).To(Equal(pod.UID))
	})

	It("Should keep an externally patched pod label across an in-place update", func() {
		rbgName := "pd-switch-inplace"
		Expect(testutil.K8sClient.Create(testutil.Ctx, newRBG(rbgName, "unassigned"))).Should(Succeed())

		pod := waitForPod(rbgName)
		podKey := client.ObjectKeyFromObject(pod)
		fakeKubelet(podKey, "image-id-v1")

		patchFromOutside(pod, "labels", runtimeRoleLabelKey, "prefill")
		rollTemplate(rbgName, updatedImage, "v2")

		// The new image landing on the same UID is what proves the update went
		// in-place rather than recreating the pod.
		Eventually(func() string {
			return readPod(podKey).Spec.Containers[0].Image
		}, timeout, interval).Should(Equal(updatedImage))

		updated := readPod(podKey)
		Expect(updated.UID).To(Equal(pod.UID))
		// Without this the label could survive just because no metadata patch
		// was ever applied to the live pod.
		Expect(updated.Annotations[templateOwnedAnnotationKey]).To(Equal("v2"))
		Expect(updated.Labels[runtimeRoleLabelKey]).To(Equal("prefill"))
		Expect(updated.Annotations[runtimeAnnotationKey]).To(Equal("unassigned"))

		// Let the controller observe the update as finished, then check the
		// completion path does not restore the template value either.
		fakeKubelet(podKey, "image-id-v2")
		Eventually(inPlaceCondition(podKey), timeout, interval).Should(Equal(corev1.ConditionTrue))

		Consistently(readMeta(podKey, "labels", runtimeRoleLabelKey),
			soakWindow, interval).Should(Equal("prefill"))
		Expect(readPod(podKey).UID).To(Equal(pod.UID))
	})
})
