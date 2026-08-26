// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
)

// ===== CollisionSafeName tests =====

func TestCollisionSafeName_Deterministic(t *testing.T) {
	uid := types.UID("uid-1234")
	a := CollisionSafeName("Deployment", "castai-agent", "nginx", uid)
	b := CollisionSafeName("Deployment", "castai-agent", "nginx", uid)
	assert.Equal(t, a, b)
}

func TestCollisionSafeName_UIDDifferentiates(t *testing.T) {
	a := CollisionSafeName("Deployment", "castai-agent", "nginx", types.UID("uid-a"))
	b := CollisionSafeName("Deployment", "castai-agent", "nginx", types.UID("uid-b"))
	assert.NotEqual(t, a, b)
}

func TestCollisionSafeName_LongName_Truncated(t *testing.T) {
	uid := types.UID("uid-9876")
	long := "postgres-primary-with-a-very-long-name-that-exceeds-limits"
	name := CollisionSafeName("StatefulSet", "payments", long, uid)
	assert.LessOrEqual(t, len(name), MaxCRDNameLen)
}

func TestCollisionSafeName_DNS1123Compliant(t *testing.T) {
	cases := []struct {
		kind, name string
		uid        types.UID
	}{
		{"Deployment", "nginx", "uid-1234"},
		{"StatefulSet", "a-very-long-name-that-exceeds-default-limits-and-must-be-truncated", "uid-9999"},
		{"DaemonSet", "fluentd", "uid-aaaa"},
	}
	for _, c := range cases {
		got := CollisionSafeName(c.kind, "ns", c.name, c.uid)
		assert.True(t, IsDNS1123Label(got), "%q is not DNS-1123: %q", c.name, got)
	}
}

// ===== Concrete in-memory fake client =====

type fakeStore map[string]*workloadsv1.TSCOriginal

func keyOf(ns, name string) string { return ns + "/" + name }

func fakeGet(store fakeStore, ns, name string) (*workloadsv1.TSCOriginal, error) {
	v, ok := store[keyOf(ns, name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "workloads.cast.ai", Resource: "tscoriginals"}, name)
	}
	return v, nil
}

func newTestSnapshot(uid types.UID, name string, conditions []metav1.Condition) *workloadsv1.TSCOriginal {
	return &workloadsv1.TSCOriginal{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "castai-agent"},
		Spec: workloadsv1.TSCOriginalSpec{
			TargetRef: workloadsv1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "castai-agent", Name: "nginx", UID: uid},
			CapturedAt: metav1.Now(),
		},
		Status: workloadsv1.TSCOriginalStatus{Conditions: conditions},
	}
}

// ===== CaptureIfAbsent tests =====

func TestCaptureIfAbsent_NewSnapshot_Creates(t *testing.T) {
	store := fakeStore{}
	identity := WorkloadIdentity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "castai-agent", Name: "nginx", UID: types.UID("uid-1"), Generation: 5}
	name := CollisionSafeName(identity.Kind, identity.Namespace, identity.Name, identity.UID)
	called := 0
	err := CaptureIfAbsent(context.Background(),
		&tscClient{store: store}, TSCOriginalAccessor, NopLogger{}, "castai-agent",
		FinalizerName("tsc-controller"), identity,
		func(id WorkloadIdentity) (*workloadsv1.TSCOriginal, error) {
			called++
			return newTestSnapshot(id.UID, name, nil), nil
		})
	require.NoError(t, err)
	assert.Equal(t, 1, called)
	assert.Len(t, store, 1)
	assert.True(t, IsReady(store[keyOf("castai-agent", name)].Status.Conditions))
}

func TestCaptureIfAbsent_ReadyTrue_Skipped(t *testing.T) {
	identity := WorkloadIdentity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "castai-agent", Name: "nginx", UID: types.UID("uid-1"), Generation: 5}
	name := CollisionSafeName(identity.Kind, identity.Namespace, identity.Name, identity.UID)
	pre := newTestSnapshot(identity.UID, name,
		[]metav1.Condition{{Type: ConditionReady, Status: metav1.ConditionTrue}})
	store := fakeStore{keyOf("castai-agent", name): pre}
	called := 0
	err := CaptureIfAbsent(context.Background(),
		&tscClient{store: store}, TSCOriginalAccessor, NopLogger{}, "castai-agent",
		FinalizerName("tsc-controller"), identity,
		func(id WorkloadIdentity) (*workloadsv1.TSCOriginal, error) {
			called++
			return newTestSnapshot(id.UID, name, nil), nil
		})
	require.NoError(t, err)
	assert.Equal(t, 0, called, "captureFn must not be called when existing snapshot is Ready")
}

func TestCaptureIfAbsent_RolledBackTrue_ReplacedNotSkipped(t *testing.T) {
	identity := WorkloadIdentity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "castai-agent", Name: "nginx", UID: types.UID("uid-1"), Generation: 5}
	name := CollisionSafeName(identity.Kind, identity.Namespace, identity.Name, identity.UID)
	pre := newTestSnapshot(identity.UID, name,
		[]metav1.Condition{
			{Type: ConditionReady, Status: metav1.ConditionTrue},
			{Type: ConditionRolledBack, Status: metav1.ConditionTrue},
		})
	store := fakeStore{keyOf("castai-agent", name): pre}
	called := 0
	err := CaptureIfAbsent(context.Background(),
		&tscClient{store: store}, TSCOriginalAccessor, NopLogger{}, "castai-agent",
		FinalizerName("tsc-controller"), identity,
		func(id WorkloadIdentity) (*workloadsv1.TSCOriginal, error) {
			called++
			return newTestSnapshot(id.UID, name, nil), nil
		})
	require.NoError(t, err)
	assert.Equal(t, 1, called, "captureFn must be called after RolledBack delete-and-recreate")
	assert.False(t, IsRolledBack(store[keyOf("castai-agent", name)].Status.Conditions))
}

func TestCaptureIfAbsent_PartialWrite_Overwritten(t *testing.T) {
	identity := WorkloadIdentity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "castai-agent", Name: "nginx", UID: types.UID("uid-1"), Generation: 5}
	name := CollisionSafeName(identity.Kind, identity.Namespace, identity.Name, identity.UID)
	pre := newTestSnapshot(identity.UID, name, nil) // no conditions → Ready=false
	store := fakeStore{keyOf("castai-agent", name): pre}
	called := 0
	err := CaptureIfAbsent(context.Background(),
		&tscClient{store: store}, TSCOriginalAccessor, NopLogger{}, "castai-agent",
		FinalizerName("tsc-controller"), identity,
		func(id WorkloadIdentity) (*workloadsv1.TSCOriginal, error) {
			called++
			return newTestSnapshot(id.UID, name, nil), nil
		})
	require.NoError(t, err)
	assert.Equal(t, 1, called)
	assert.True(t, IsReady(store[keyOf("castai-agent", name)].Status.Conditions))
}

func TestCaptureIfAbsent_CaptureFuncError_Propagates(t *testing.T) {
	store := fakeStore{}
	identity := WorkloadIdentity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "castai-agent", Name: "nginx", UID: types.UID("uid-1"), Generation: 5}
	boom := errors.New("boom")
	err := CaptureIfAbsent(context.Background(),
		&tscClient{store: store}, TSCOriginalAccessor, NopLogger{}, "castai-agent",
		FinalizerName("tsc-controller"), identity,
		func(id WorkloadIdentity) (*workloadsv1.TSCOriginal, error) {
			return nil, boom
		})
	require.Error(t, err)
	assert.Len(t, store, 0)
}

// ===== Rollback tests =====

func TestRollback_UIDMismatch_MarksTargetGone(t *testing.T) {
	identity := WorkloadIdentity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "castai-agent", Name: "nginx", UID: types.UID("uid-1"), Generation: 5}
	name := CollisionSafeName(identity.Kind, identity.Namespace, identity.Name, identity.UID)
	pre := newTestSnapshot(identity.UID, name,
		[]metav1.Condition{{Type: ConditionReady, Status: metav1.ConditionTrue}})
	pre.Finalizers = []string{FinalizerName("tsc-controller")}
	store := fakeStore{keyOf("castai-agent", name): pre}

	inverseCalled := 0
	err := Rollback(context.Background(),
		&tscClient{store: store}, TSCOriginalAccessor, NopLogger{}, "castai-agent",
		FinalizerName("tsc-controller"),
		func(_ context.Context, snap *workloadsv1.TSCOriginal) (string, bool, error) {
			return "different-uid", true, nil
		},
		func(_ context.Context, _ *workloadsv1.TSCOriginal) error {
			inverseCalled++
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, 0, inverseCalled)
	assert.True(t, IsRolledBack(store[keyOf("castai-agent", name)].Status.Conditions))
	assert.Equal(t, ReasonTargetGone, reasonOf(store[keyOf("castai-agent", name)].Status.Conditions, ConditionRolledBack))
	assert.Empty(t, store[keyOf("castai-agent", name)].Finalizers)
}

func TestRollback_InversePatchError_ContinuesRest(t *testing.T) {
	goodName := CollisionSafeName("Deployment", "castai-agent", "good", types.UID("uid-good"))
	badName := CollisionSafeName("Deployment", "castai-agent", "bad", types.UID("uid-bad"))
	good := newTestSnapshot(types.UID("uid-good"), goodName,
		[]metav1.Condition{{Type: ConditionReady, Status: metav1.ConditionTrue}})
	bad := newTestSnapshot(types.UID("uid-bad"), badName,
		[]metav1.Condition{{Type: ConditionReady, Status: metav1.ConditionTrue}})
	store := fakeStore{
		keyOf("castai-agent", goodName): good,
		keyOf("castai-agent", badName):  bad,
	}
	inverseErr := errors.New("inverse failed")
	err := Rollback(context.Background(),
		&tscClient{store: store}, TSCOriginalAccessor, NopLogger{}, "castai-agent",
		FinalizerName("tsc-controller"),
		func(_ context.Context, snap *workloadsv1.TSCOriginal) (string, bool, error) {
			return string(snap.Spec.TargetRef.UID), true, nil
		},
		func(_ context.Context, snap *workloadsv1.TSCOriginal) error {
			if snap.Name == badName {
				return inverseErr
			}
			return nil
		})
	require.Error(t, err)
	assert.True(t, IsRolledBack(store[keyOf("castai-agent", goodName)].Status.Conditions))
	assert.False(t, IsRolledBack(store[keyOf("castai-agent", badName)].Status.Conditions))
}

func TestRollback_AlreadyRolledBack_Skipped(t *testing.T) {
	identity := WorkloadIdentity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "castai-agent", Name: "nginx", UID: types.UID("uid-1"), Generation: 5}
	name := CollisionSafeName(identity.Kind, identity.Namespace, identity.Name, identity.UID)
	pre := newTestSnapshot(identity.UID, name,
		[]metav1.Condition{
			{Type: ConditionReady, Status: metav1.ConditionTrue},
			{Type: ConditionRolledBack, Status: metav1.ConditionTrue},
		})
	store := fakeStore{keyOf("castai-agent", name): pre}
	inverseCalled := 0
	err := Rollback(context.Background(),
		&tscClient{store: store}, TSCOriginalAccessor, NopLogger{}, "castai-agent",
		FinalizerName("tsc-controller"),
		func(_ context.Context, snap *workloadsv1.TSCOriginal) (string, bool, error) {
			return string(snap.Spec.TargetRef.UID), true, nil
		},
		func(_ context.Context, _ *workloadsv1.TSCOriginal) error {
			inverseCalled++
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, 0, inverseCalled)
}

// ===== Serialization roundtrip =====

func TestSerializationRoundtrip_TSCOriginal_NilVsEmpty(t *testing.T) {
	// The presence flag is the load-bearing distinguisher (see types.go docs).
	// The slice itself may unmarshal as nil in both cases because
	// encoding/json omitempty drops empty slices — the Present bit is the
	// source of truth at rollback time.
	nilCase := &workloadsv1.TSCOriginal{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "y"},
		Spec: workloadsv1.TSCOriginalSpec{
			TargetRef:           workloadsv1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "y", Name: "n", UID: "u"},
			OriginalTSCs:        nil,
			OriginalTSCsPresent: false,
			CapturedAt:          metav1.Now(),
		},
	}
	emptyCase := nilCase.DeepCopy()
	emptyCase.Spec.OriginalTSCs = []corev1.TopologySpreadConstraint{}
	emptyCase.Spec.OriginalTSCsPresent = true

	dataNil, err := json.Marshal(nilCase)
	require.NoError(t, err)
	dataEmpty, err := json.Marshal(emptyCase)
	require.NoError(t, err)
	// Present flags must differ in serialized output.
	assert.Contains(t, string(dataEmpty), `"originalTSCsPresent":true`)
	assert.Contains(t, string(dataNil), `"originalTSCsPresent":false`)

	var rtNil workloadsv1.TSCOriginal
	var rtEmpty workloadsv1.TSCOriginal
	require.NoError(t, json.Unmarshal(dataNil, &rtNil))
	require.NoError(t, json.Unmarshal(dataEmpty, &rtEmpty))
	assert.Nil(t, rtNil.Spec.OriginalTSCs)
	assert.False(t, rtNil.Spec.OriginalTSCsPresent)
	assert.True(t, rtEmpty.Spec.OriginalTSCsPresent)
}

// ===== tscClient — concrete Client[*TSCOriginal] =====

type tscClient struct {
	store fakeStore
}

func (c *tscClient) Get(_ context.Context, ns, name string) (*workloadsv1.TSCOriginal, error) {
	return fakeGet(c.store, ns, name)
}

func (c *tscClient) Create(_ context.Context, ns string, obj *workloadsv1.TSCOriginal) (*workloadsv1.TSCOriginal, error) {
	c.store[keyOf(ns, obj.Name)] = obj
	return obj, nil
}

func (c *tscClient) Update(_ context.Context, ns string, obj *workloadsv1.TSCOriginal) (*workloadsv1.TSCOriginal, error) {
	c.store[keyOf(ns, obj.Name)] = obj
	return obj, nil
}

func (c *tscClient) UpdateStatus(_ context.Context, ns string, obj *workloadsv1.TSCOriginal) (*workloadsv1.TSCOriginal, error) {
	c.store[keyOf(ns, obj.Name)] = obj
	return obj, nil
}

func (c *tscClient) Delete(_ context.Context, ns, name string) error {
	delete(c.store, keyOf(ns, name))
	return nil
}

func (c *tscClient) List(_ context.Context, ns string) ([]*workloadsv1.TSCOriginal, error) {
	out := []*workloadsv1.TSCOriginal{}
	for k, v := range c.store {
		if len(k) > len(ns)+1 && k[:len(ns)+1] == ns+"/" {
			out = append(out, v)
		}
	}
	return out, nil
}

func (c *tscClient) Patch(_ context.Context, ns, name string, _ PatchType, data []byte) (*workloadsv1.TSCOriginal, error) {
	v, err := fakeGet(c.store, ns, name)
	if err != nil {
		return nil, err
	}
	applyFinalizerMergePatch(v, data)
	return v, nil
}

func applyFinalizerMergePatch(obj *workloadsv1.TSCOriginal, data []byte) {
	var p struct {
		Metadata struct {
			Finalizers []string `json:"finalizers"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return
	}
	obj.Finalizers = p.Metadata.Finalizers
}

func reasonOf(conds []metav1.Condition, t string) string {
	for _, c := range conds {
		if c.Type == t {
			return c.Reason
		}
	}
	return ""
}
