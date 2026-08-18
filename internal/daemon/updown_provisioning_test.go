package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
	"github.com/randax/talos-box/internal/provision"
)

func intPointer(value int) *int { return &value }

func TestCreateFromSpecWithoutCNIUsesLegacyProvisioningFields(t *testing.T) {
	spec := config.ClusterSpec{Name: "demo"}
	args := createArgsFromSpec(spec, false)
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cni", "csi", "lb", "bgp", "hubble"} {
		if _, found := fields[key]; found {
			t.Fatalf("createFromSpec legacy input unexpectedly includes %q: %s", key, encoded)
		}
	}
}

func TestCreateFromSpecPreservesCSIIntentOnTheWire(t *testing.T) {
	spec := config.ClusterSpec{
		Name: "demo",
		ProvisioningIntent: cluster.ProvisioningIntent{
			CNI: cluster.CNICilium, CSI: cluster.CSILonghorn, LB: true,
		},
	}
	encoded, err := json.Marshal(createArgsFromSpec(spec, false))
	if err != nil {
		t.Fatal(err)
	}
	var input cluster.ProvisioningIntentInput
	if err := json.Unmarshal(encoded, &input); err != nil {
		t.Fatal(err)
	}
	intent, err := input.Intent()
	if err != nil {
		t.Fatal(err)
	}
	if intent != spec.ProvisioningIntent {
		t.Fatalf("wire intent = %+v, want %+v", intent, spec.ProvisioningIntent)
	}
}

func TestReconcileProvisioningIntentMutationRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		current        cluster.ProvisioningIntent
		desired        cluster.ProvisioningIntent
		allMaintenance bool
		want           cluster.ProvisioningIntent
		wantChanged    bool
		wantErr        string
	}{
		{
			name:    "provisioned CNI cannot change",
			current: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			desired: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
			wantErr: "tbx cluster destroy demo && tbx up",
		},
		{
			name:    "provisioned CNI cannot be removed",
			current: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			wantErr: "tbx cluster destroy demo && tbx up",
		},
		{
			name:           "add CNI while every node is in maintenance",
			desired:        cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			allMaintenance: true,
			want:           cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			wantChanged:    true,
		},
		{
			name:    "adding CNI after configuration is rejected",
			desired: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			wantErr: "all nodes are in maintenance",
		},
		{
			name:        "enable LoadBalancer later",
			current:     cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
			desired:     cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			want:        cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			wantChanged: true,
		},
		{
			name:    "disable LoadBalancer is rejected",
			current: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			desired: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
			wantErr: "lb is immutable once enabled",
		},
		{
			name:        "Hubble remains symmetric",
			current:     cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, Hubble: true},
			desired:     cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
			want:        cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
			wantChanged: true,
		},
		{
			name:        "BGP becomes enabled declaratively",
			current:     cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
			desired:     cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true},
			want:        cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true},
			wantChanged: true,
		},
		{
			name:        "BGP becomes disabled declaratively",
			current:     cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true},
			desired:     cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
			want:        cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
			wantChanged: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := cluster.Cluster{Name: "demo", ProvisioningIntent: test.current}
			got, changed, err := reconcileProvisioningIntent(item, config.ClusterSpec{Name: item.Name, ProvisioningIntent: test.desired}, test.allMaintenance)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("reconcileProvisioningIntent() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want || changed != test.wantChanged {
				t.Fatalf("reconcileProvisioningIntent() = (%+v, %t), want (%+v, %t)", got, changed, test.want, test.wantChanged)
			}
		})
	}
}

func TestReconcileProvisioningIntentStorageLifecycleRules(t *testing.T) {
	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}}
	tests := []struct {
		name    string
		current cluster.CSI
		desired cluster.CSI
		count   *int
		changed bool
		want    cluster.CSI
		errText []string
	}{
		{name: "add later", desired: cluster.CSILocalPath, changed: true, want: cluster.CSILocalPath},
		{name: "same engine no-op", current: cluster.CSILonghorn, desired: cluster.CSILonghorn, want: cluster.CSILonghorn},
		{name: "switch after zero volumes", current: cluster.CSILocalPath, desired: cluster.CSILonghorn, count: intPointer(0), changed: true, want: cluster.CSILonghorn},
		{name: "remove after zero volumes", current: cluster.CSILonghorn, count: intPointer(0), changed: true},
		{name: "switch requires observation", current: cluster.CSILocalPath, desired: cluster.CSILonghorn, errText: []string{"volume count is verified"}},
		{name: "volumes block switch", current: cluster.CSILocalPath, desired: cluster.CSILonghorn, count: intPointer(2), errText: []string{"delete the volumes first", "tbx cluster destroy demo"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item.CSI = test.current
			desired := config.ClusterSpec{Name: item.Name, ProvisioningIntent: item.ProvisioningIntent}
			desired.CSI = test.desired
			got, changed, err := reconcileProvisioningIntentWithVolumes(item, desired, false, test.count)
			if len(test.errText) > 0 {
				if err == nil {
					t.Fatal("expected storage lifecycle error")
				}
				for _, text := range test.errText {
					if !strings.Contains(err.Error(), text) {
						t.Fatalf("error = %q, want %q", err, text)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if changed != test.changed || got.CSI != test.want {
				t.Fatalf("result = (%q, %t), want (%q, %t)", got.CSI, changed, test.want, test.changed)
			}
		})
	}
}

func TestStorageTeardownFailureLeavesOldIntentForConvergentRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	deleteCalls := 0
	service := &Server{
		destroyVolumeCount:    func(context.Context, cluster.Cluster) (int, error) { return 0, nil },
		storageEngineValidate: func(context.Context, cluster.Cluster) error { return nil },
		storageEngineDelete: func(context.Context, cluster.Cluster) error {
			deleteCalls++
			if deleteCalls == 1 {
				return context.Canceled
			}
			return nil
		},
	}
	raw, err := json.Marshal(upArgs{Clusters: []config.ClusterSpec{{Name: item.Name, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn}}}})
	if err != nil {
		t.Fatal(err)
	}
	observations := map[string]storageObservation{item.Name: {engine: item.CSI, count: 0, running: map[string]bool{item.Nodes[0].Name: false}}}
	if err := service.deleteUpStorageTransitions(raw, observations); err == nil {
		t.Fatal("expected interrupted teardown")
	}
	stored, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CSI != cluster.CSILocalPath {
		t.Fatalf("CSI after interrupted teardown = %q, want old intent", stored.CSI)
	}
	if err := service.deleteUpStorageTransitions(raw, observations); err != nil {
		t.Fatalf("idempotent teardown retry: %v", err)
	}
}

func TestUpAddsAndSwitchesCSIWithoutChangingMachineConfig(t *testing.T) {
	for _, test := range []struct {
		name       string
		current    cluster.CSI
		desired    cluster.CSI
		wantDelete int
	}{
		{name: "add later", desired: cluster.CSILocalPath},
		{name: "switch after zero volumes", current: cluster.CSILocalPath, desired: cluster.CSILonghorn, wantDelete: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
			if err != nil {
				t.Fatal(err)
			}
			item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: test.current}
			if err := cluster.Save(item); err != nil {
				t.Fatal(err)
			}
			dir, err := cluster.Dir(item.Name)
			if err != nil {
				t.Fatal(err)
			}
			machineConfig := filepath.Join(dir, item.Nodes[0].Name+".yaml")
			const marker = "machine-config-must-not-change\n"
			if err := os.WriteFile(machineConfig, []byte(marker), 0o600); err != nil {
				t.Fatal(err)
			}
			deletes := 0
			service := &Server{
				vms:                   map[string]map[string]hypervisor.Machine{item.Name: {item.Nodes[0].Name: &fakeMachine{active: true}}},
				destroyVolumeCount:    func(context.Context, cluster.Cluster) (int, error) { return 0, nil },
				storageEngineValidate: func(context.Context, cluster.Cluster) error { return nil },
				storageEngineDelete: func(_ context.Context, old cluster.Cluster) error {
					deletes++
					if old.CSI != test.current {
						t.Fatalf("deleted engine = %q, want %q", old.CSI, test.current)
					}
					return nil
				},
				provisionReconcile: func(_ context.Context, request provision.Request) (provision.Result, error) {
					if request.Cluster.CSI != test.desired {
						t.Fatalf("reconciled CSI = %q, want %q", request.Cluster.CSI, test.desired)
					}
					return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
				},
			}
			raw, err := json.Marshal(upArgs{Clusters: []config.ClusterSpec{{
				Name: item.Name, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: test.desired},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			response := service.dispatchProvisioning(Request{Op: "up", Args: raw}, nil)
			if !response.OK {
				t.Fatalf("up failed: %s", response.Error)
			}
			stored, err := cluster.Load(item.Name)
			if err != nil {
				t.Fatal(err)
			}
			if stored.CSI != test.desired || deletes != test.wantDelete {
				t.Fatalf("stored CSI/deletes = %q/%d, want %q/%d", stored.CSI, deletes, test.desired, test.wantDelete)
			}
			contents, err := os.ReadFile(machineConfig)
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != marker {
				t.Fatalf("machine config changed while adopting CSI: %q", contents)
			}
		})
	}
}

func TestStorageTransitionsValidateEveryClusterBeforeAnyDelete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var specs []config.ClusterSpec
	observations := make(map[string]storageObservation)
	for index, name := range []string{"first", "second"} {
		item, err := cluster.New(name, index, 1, 0, cluster.NodeDefaults{})
		if err != nil {
			t.Fatal(err)
		}
		item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath}
		if err := cluster.Save(item); err != nil {
			t.Fatal(err)
		}
		specs = append(specs, config.ClusterSpec{Name: name, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn}})
		observations[name] = storageObservation{engine: cluster.CSILocalPath, count: 0, running: map[string]bool{item.Nodes[0].Name: false}}
	}
	deletes := 0
	service := &Server{
		destroyVolumeCount: func(context.Context, cluster.Cluster) (int, error) { return 0, nil },
		storageEngineValidate: func(_ context.Context, item cluster.Cluster) error {
			if item.Name == "second" {
				return errors.New("unmanaged storage collision")
			}
			return nil
		},
		storageEngineDelete: func(context.Context, cluster.Cluster) error {
			deletes++
			return nil
		},
	}
	raw, err := json.Marshal(upArgs{Clusters: specs})
	if err != nil {
		t.Fatal(err)
	}
	err = service.deleteUpStorageTransitions(raw, observations)
	if err == nil || !strings.Contains(err.Error(), "unmanaged storage collision") {
		t.Fatalf("deleteUpStorageTransitions() error = %v", err)
	}
	if deletes != 0 {
		t.Fatalf("delete calls = %d, want zero before every target validates", deletes)
	}
}

func TestPreflightUpRejectsEveryInvalidClusterBeforeAnyIntentIsPersisted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	first, err := cluster.New("first", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(first); err != nil {
		t.Fatal(err)
	}
	second, err := cluster.New("second", 1, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	second.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}
	if err := cluster.Save(second); err != nil {
		t.Fatal(err)
	}

	service := &Server{}
	_, err = service.preflightUp([]config.ClusterSpec{
		{Name: first.Name, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}},
		{Name: second.Name, ProvisioningIntent: cluster.ProvisioningIntent{}},
	}, map[string]ClusterState{first.Name: {Exists: true}, second.Name: {Exists: true}}, map[string]maintenanceObservation{
		first.Name: {
			running: map[string]bool{first.Nodes[0].Name: false},
			phases:  map[string]provision.Phase{first.Nodes[0].Name: provision.PhaseMaintenance},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "tbx cluster destroy second && tbx up") {
		t.Fatalf("preflightUp() error = %v, want immutable-CNI error", err)
	}
	updated, err := cluster.Load(first.Name)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CNI != "" {
		t.Fatalf("preflight persisted first cluster before second failed: %+v", updated.ProvisioningIntent)
	}
}

func TestClusterStateRemainsIntentOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(filepath.Join(dir, "cluster.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"progress", "stage", "bootstrap", "provisioned"} {
		if strings.Contains(string(state), "\""+forbidden+"\"") {
			t.Fatalf("cluster state records provisioning progress %q:\n%s", forbidden, state)
		}
	}
}

func TestPersistIntentUpdatesDefersHostBGPDisableUntilL2Reconciliation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	next := item
	next.BGP = false
	if err := persistIntentUpdates([]intentUpdate{{next: next}}); err != nil {
		t.Fatal(err)
	}
	updated, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if updated.BGP {
		t.Fatalf("persisted intent = %+v, want BGP disabled", updated.ProvisioningIntent)
	}
}

func TestCreateArgsFromSpecCarriesPerClusterTalos(t *testing.T) {
	// Two clusters in one file pinning different versions/schematics must each
	// produce create args for their own image (issue #202).
	specs := []config.ClusterSpec{
		{Name: "stable", Talos: config.TalosSpec{Version: "v1.13.6", Schematic: "aaa"}},
		{Name: "canary", Talos: config.TalosSpec{Version: "v1.14.0", Schematic: "bbb", Extensions: []string{"gvisor"}}},
	}
	for _, spec := range specs {
		args := createArgsFromSpec(spec, false)
		if args.Version != spec.Talos.Version || args.Schematic != spec.Talos.Schematic {
			t.Errorf("%s create args image = (%q, %q), want (%q, %q)",
				spec.Name, args.Version, args.Schematic, spec.Talos.Version, spec.Talos.Schematic)
		}
		if !reflect.DeepEqual(args.Extensions, spec.Talos.Extensions) {
			t.Errorf("%s create args extensions = %#v, want %#v", spec.Name, args.Extensions, spec.Talos.Extensions)
		}
	}
}

func TestResolveSpecTalosFallsBackToFileTalosForOlderClients(t *testing.T) {
	fileTalos := config.TalosSpec{Version: "v1.13.6", Schematic: "aaa"}
	// An older tbx never resolved a per-cluster spec: the cluster arrives with
	// a zero Talos and the request-level file talos must apply.
	legacy := config.ClusterSpec{Name: "demo"}
	if got := resolveSpecTalos(legacy, fileTalos); !got.Equal(fileTalos) {
		t.Errorf("legacy fallback = %#v, want %#v", got, fileTalos)
	}
	// A resolved per-cluster spec wins over the request-level file talos.
	pinned := config.ClusterSpec{Name: "demo", Talos: config.TalosSpec{Version: "v1.14.0"}}
	if got := resolveSpecTalos(pinned, fileTalos); !got.Equal(pinned.Talos) {
		t.Errorf("resolved spec = %#v, want %#v", got, pinned.Talos)
	}
	// An explicit extensions opt-out is a resolved spec, not a zero one.
	optOut := config.ClusterSpec{Name: "demo", Talos: config.TalosSpec{Extensions: []string{}}}
	if got := resolveSpecTalos(optOut, fileTalos); !got.Equal(optOut.Talos) {
		t.Errorf("opt-out spec = %#v, want %#v", got, optOut.Talos)
	}
}

func TestUpCreatesEachClusterFromItsOwnImage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	images := map[string]string{"stable": "disk-aaa", "canary": "disk-bbb"}
	// canary re-composes its brought schematic with an extension: the recorded
	// composition is what keeps this create offline, and its image lives under
	// the composed id.
	const canaryComposed = "bbb-composed"
	if err := imagecache.New(root).RecordComposition("bbb", "v1.14.0", []string{"gvisor"}, canaryComposed); err != nil {
		t.Fatal(err)
	}
	for _, seed := range []struct{ schematic, version, body string }{
		{"aaa", "v1.13.6", images["stable"]},
		{canaryComposed, "v1.14.0", images["canary"]},
	} {
		path := filepath.Join(root, seed.schematic, seed.version, "arm64", "disk.raw")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(seed.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	service := &Server{
		cache:        imagecache.New(root),
		hypervisor:   &fakeHypervisor{architecture: hypervisor.ArchitectureARM64},
		vms:          make(map[string]map[string]hypervisor.Machine),
		helperCheck:  func() error { return nil },
		hostPressure: func(string) (hostpressure.Snapshot, error) { return hostpressure.Snapshot{}, nil },
	}
	raw, err := json.Marshal(upArgs{Clusters: []config.ClusterSpec{
		{Name: "stable", ControlPlanes: 1, Workers: 0, Node: cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1},
			Talos: config.TalosSpec{Version: "v1.13.6", Schematic: "aaa"}},
		{Name: "canary", ControlPlanes: 1, Workers: 0, Node: cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1},
			Talos: config.TalosSpec{Version: "v1.14.0", Schematic: "bbb", Extensions: []string{"gvisor"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	actions, err := service.up(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[0].Kind != ActionCreate || actions[1].Kind != ActionCreate {
		t.Fatalf("up actions = %+v, want two creates", actions)
	}

	for name, wantDisk := range images {
		item, err := cluster.Load(name)
		if err != nil {
			t.Fatal(err)
		}
		wantVersion, wantSchematic := "v1.13.6", "aaa"
		if name == "canary" {
			wantVersion, wantSchematic = "v1.14.0", canaryComposed
		}
		if item.TalosVersion != wantVersion || item.Schematic != wantSchematic {
			t.Fatalf("%s state = (%q, %q), want (%q, %q)", name, item.TalosVersion, item.Schematic, wantVersion, wantSchematic)
		}
		dir, err := cluster.Dir(name)
		if err != nil {
			t.Fatal(err)
		}
		disk, err := os.Open(filepath.Join(dir, item.Nodes[0].Name+".img"))
		if err != nil {
			t.Fatal(err)
		}
		// The node disk is the cached image followed by sparse padding; only
		// the image-sized prefix identifies which image seeded it.
		prefix := make([]byte, len(wantDisk))
		_, err = disk.ReadAt(prefix, 0)
		_ = disk.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(prefix) != wantDisk {
			t.Fatalf("%s node disk provisioned from %q, want %q", name, prefix, wantDisk)
		}
	}
	canary, err := cluster.Load("canary")
	if err != nil {
		t.Fatal(err)
	}
	if len(canary.TalosExtensions) != 1 || canary.TalosExtensions[0] != "gvisor" {
		t.Fatalf("canary extensions = %#v, want [gvisor]", canary.TalosExtensions)
	}
	if canary.BaseSchematic != "bbb" {
		t.Fatalf("canary base schematic = %q, want %q", canary.BaseSchematic, "bbb")
	}
}

// A cluster talosbox.yaml names is config-managed from that up onwards, so a
// cluster created imperatively (or by a tbx predating the flag) stops being
// told to destroy and recreate once a file backs it (#267).
func TestPreflightUpClaimsExistingClusterAsConfigManaged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}

	service := &Server{}
	updates, err := service.preflightUp(
		[]config.ClusterSpec{{Name: item.Name, ProvisioningIntent: item.ProvisioningIntent}},
		map[string]ClusterState{item.Name: {Exists: true}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistIntentUpdates(updates); err != nil {
		t.Fatal(err)
	}
	claimed, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !claimed.ConfigManaged {
		t.Fatal("preflightUp() left an up-managed cluster unclaimed")
	}
	if claimed.ProvisioningIntent != item.ProvisioningIntent {
		t.Fatalf("claim changed intent: %+v, want %+v", claimed.ProvisioningIntent, item.ProvisioningIntent)
	}
}

func TestCreateFromSpecMarksClusterConfigManaged(t *testing.T) {
	if !createArgsFromSpec(config.ClusterSpec{Name: "demo"}, false).ConfigManaged {
		t.Fatal("createArgsFromSpec() did not mark the create as config-managed")
	}
	encoded, err := json.Marshal(createArgsFromSpec(config.ClusterSpec{Name: "demo"}, false))
	if err != nil {
		t.Fatal(err)
	}
	var decoded createArgs
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.ConfigManaged {
		t.Fatalf("createArgs lost its config-managed provenance on the wire: %s", encoded)
	}
}
