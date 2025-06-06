// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	v1 "github.com/openshift/api/machine/v1"
	machinev1 "github.com/openshift/client-go/machine/applyconfigurations/machine/v1"
	typedmachinev1 "github.com/openshift/client-go/machine/clientset/versioned/typed/machine/v1"
	gentype "k8s.io/client-go/gentype"
)

// fakeControlPlaneMachineSets implements ControlPlaneMachineSetInterface
type fakeControlPlaneMachineSets struct {
	*gentype.FakeClientWithListAndApply[*v1.ControlPlaneMachineSet, *v1.ControlPlaneMachineSetList, *machinev1.ControlPlaneMachineSetApplyConfiguration]
	Fake *FakeMachineV1
}

func newFakeControlPlaneMachineSets(fake *FakeMachineV1, namespace string) typedmachinev1.ControlPlaneMachineSetInterface {
	return &fakeControlPlaneMachineSets{
		gentype.NewFakeClientWithListAndApply[*v1.ControlPlaneMachineSet, *v1.ControlPlaneMachineSetList, *machinev1.ControlPlaneMachineSetApplyConfiguration](
			fake.Fake,
			namespace,
			v1.SchemeGroupVersion.WithResource("controlplanemachinesets"),
			v1.SchemeGroupVersion.WithKind("ControlPlaneMachineSet"),
			func() *v1.ControlPlaneMachineSet { return &v1.ControlPlaneMachineSet{} },
			func() *v1.ControlPlaneMachineSetList { return &v1.ControlPlaneMachineSetList{} },
			func(dst, src *v1.ControlPlaneMachineSetList) { dst.ListMeta = src.ListMeta },
			func(list *v1.ControlPlaneMachineSetList) []*v1.ControlPlaneMachineSet {
				return gentype.ToPointerSlice(list.Items)
			},
			func(list *v1.ControlPlaneMachineSetList, items []*v1.ControlPlaneMachineSet) {
				list.Items = gentype.FromPointerSlice(items)
			},
		),
		fake,
	}
}
