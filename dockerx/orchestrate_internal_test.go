package dockerx

import (
	"testing"

	"github.com/gitproduct/docker-sandbox/sandbox"
)

func TestRuntimeAndInstanceMappings(t *testing.T) {
	spec := sandbox.Spec{InstanceID: "box1", UserID: "alice", Image: "alpine", SkipPull: true}
	encoded, err := sandbox.EncodeSpecChecked(spec)
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{
		sandbox.LabelRole:      sandbox.RoleInstance,
		sandbox.LabelInstance:  "box1",
		sandbox.LabelUser:      "alice",
		sandbox.LabelNetwork:   "sandbox-net-alice",
		sandbox.LabelVolume:    "sandbox-ws-box1",
		sandbox.LabelHostPorts: "20000,20001",
		sandbox.LabelSpec:      encoded,
	}
	info := ContainerInspect{
		ID:     "cid",
		Name:   "/sandbox-box1",
		Status: "RUNNING",
		Image:  "alpine",
		Labels: labels,
	}
	runtime := runtimeFromInspect(info)
	if runtime.ID != "cid" || runtime.Name != "sandbox-box1" || runtime.Status != "running" || len(runtime.HostPorts) != 2 {
		t.Fatalf("runtime=%+v", runtime)
	}
	inst := instanceFromInspect(info)
	if inst.ID != "box1" || inst.UserID != "alice" || !inst.Spec.SkipPull || len(inst.HostPorts) != 2 {
		t.Fatalf("instance=%+v", inst)
	}

	summary := ContainerSummary{
		ID:     "cid",
		Names:  []string{"/sandbox-box1"},
		Status: "RUNNING",
		Image:  "alpine",
		Labels: labels,
	}
	fromSummary, ok := instanceFromSummary(summary)
	if !ok || fromSummary.ID != "box1" || fromSummary.Name != "sandbox-box1" {
		t.Fatalf("summary=%+v ok=%v", fromSummary, ok)
	}
	if _, ok := instanceFromSummary(ContainerSummary{}); ok {
		t.Fatal("unmanaged summary should be ignored")
	}

	fromVolume := instanceFromVolume(WorkspaceInfo{
		Name:       "sandbox-ws-box1",
		InstanceID: "box1",
		Labels:     labels,
	}, "sandbox")
	if fromVolume.ID != "box1" || fromVolume.Status != sandbox.StatusDestroyed || fromVolume.Spec.InstanceID != "box1" {
		t.Fatalf("volume instance=%+v", fromVolume)
	}
}
