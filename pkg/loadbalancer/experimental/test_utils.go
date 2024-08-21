// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package experimental

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cilium/hive/cell"
	"github.com/cilium/statedb"
	"github.com/cilium/statedb/reconciler"
	"github.com/cilium/stream"
	"github.com/pmezard/go-difflib/difflib"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sRuntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"github.com/cilium/cilium/pkg/clustermesh/types"
	"github.com/cilium/cilium/pkg/datapath/tables"
	"github.com/cilium/cilium/pkg/hive"
	"github.com/cilium/cilium/pkg/k8s"
	"github.com/cilium/cilium/pkg/k8s/resource"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	slim_fake "github.com/cilium/cilium/pkg/k8s/slim/k8s/client/clientset/versioned/fake"
	"github.com/cilium/cilium/pkg/loadbalancer"
	"github.com/cilium/cilium/pkg/maps/lbmap"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/time"
)

var (
	slimDecoder k8sRuntime.Decoder
)

func init() {
	slimScheme := k8sRuntime.NewScheme()
	slim_fake.AddToScheme(slimScheme)
	slimScheme.AddKnownTypes(slim_corev1.SchemeGroupVersion, &metav1.List{})
	slimDecoder = serializer.NewCodecFactory(slimScheme).UniversalDeserializer()
}

var (
	// special addresses that are replaced by the test runner.
	autoAddr = loadbalancer.L3n4Addr{
		AddrCluster: types.MustParseAddrCluster("0.0.0.1"),
		L4Addr:      loadbalancer.L4Addr{},
		Scope:       0,
	}
	zeroAddr = loadbalancer.L3n4Addr{
		AddrCluster: types.MustParseAddrCluster("0.0.0.3"),
		L4Addr:      loadbalancer.L4Addr{},
		Scope:       0,
	}

	extraFrontend = loadbalancer.L3n4Addr{
		AddrCluster: types.MustParseAddrCluster("10.0.0.2"),
		L4Addr: loadbalancer.L4Addr{
			Protocol: loadbalancer.TCP,
			Port:     80,
		},
		Scope: 0,
	}

	// backend addresses
	backend1 = loadbalancer.L3n4Addr{
		AddrCluster: types.MustParseAddrCluster("10.1.0.1"),
		L4Addr: loadbalancer.L4Addr{
			Protocol: loadbalancer.TCP,
			Port:     80,
		},
		Scope: 0,
	}
	backend2 = loadbalancer.L3n4Addr{
		AddrCluster: types.MustParseAddrCluster("10.1.0.2"),
		L4Addr: loadbalancer.L4Addr{
			Protocol: loadbalancer.TCP,
			Port:     80,
		},
		Scope: 0,
	}

	// frontendAddrs are assigned to the <auto>/autoAddr. Each test set is run with
	// each of these.
	frontendAddrs = []loadbalancer.L3n4Addr{
		parseAddrPort("10.0.0.1:80"),
		parseAddrPort("[2001::1]:80"),
	}

	nodePortAddrs = []netip.Addr{
		netip.MustParseAddr("10.0.0.3"),
		netip.MustParseAddr("2002::1"),
	}
)

func decodeObject[Obj k8sRuntime.Object](t *testing.T, file string) Obj {
	bytes, err := os.ReadFile(file)
	require.NoError(t, err, "ReadFile(%s)", file)
	obj, _, err := slimDecoder.Decode(bytes, nil, nil)
	require.NoError(t, err, "Decode(%s)", file)
	return obj.(Obj)
}

func readObjects[Obj k8sRuntime.Object](t *testing.T, dataDir string, prefix string) (out []Obj) {
	ents, err := os.ReadDir(dataDir)
	require.NoError(t, err, "ReadDir(%s)", dataDir)

	for _, ent := range ents {
		if strings.HasPrefix(ent.Name(), prefix) && strings.HasSuffix(ent.Name(), ".yaml") {
			out = append(out, decodeObject[Obj](t, path.Join(dataDir, ent.Name())))
		}
	}
	return
}

func UpsertEvent[Obj k8sRuntime.Object](obj Obj) resource.Event[Obj] {
	return resource.Event[Obj]{
		Object: obj,
		Key:    resource.NewKey(obj),
		Kind:   resource.Upsert,
		Done:   func(error) {},
	}
}

func DeleteEvent[Obj k8sRuntime.Object](obj Obj) resource.Event[Obj] {
	return resource.Event[Obj]{
		Object: obj,
		Key:    resource.NewKey(obj),
		Kind:   resource.Delete,
		Done:   func(error) {},
	}
}

type numeric interface {
	~int | ~uint32 | ~uint16
}

// TODO: Figure out what to do about the IDs. If we want to do fault inject the
// operations will be retried and the ID allocations are non-deterministic.
func sanitizeID[Num numeric](n Num, sanitize bool) string {
	if !sanitize {
		return strconv.FormatInt(int64(n), 10)
	}
	if n == 0 {
		return "<zero>"
	}
	return "<non-zero>"
}

func parseAddrPort(s string) loadbalancer.L3n4Addr {
	addrS, portS, found := strings.Cut(s, "]:")
	if found {
		// IPv6
		addrS = addrS[1:] // drop [
	} else {
		// IPv4
		addrS, portS, found = strings.Cut(s, ":")
		if !found {
			panic("bad <ip:port>")
		}
	}
	addr := types.MustParseAddrCluster(addrS)
	port, _ := strconv.ParseInt(portS, 10, 16)
	return *loadbalancer.NewL3n4Addr(
		loadbalancer.TCP,
		addr, uint16(port), loadbalancer.ScopeExternal,
	)

}

// MapDump is a dump of a BPF map. These are generated by the dump() method, which
// solely defines the format.
type MapDump = string

// DumpLBMaps the load-balancing maps into a concise format for assertions in tests.
func DumpLBMaps(lbmaps LBMaps, feAddr loadbalancer.L3n4Addr, sanitizeIDs bool, customIPString func(net.IP) string) (out []MapDump) {
	out = []string{}

	replaceAddr := func(addr net.IP, port uint16) (s string) {
		if addr.IsUnspecified() {
			s = "<zero>"
			return
		}
		switch addr.String() {
		case feAddr.AddrCluster.String():
			s = "<auto>"
		case nodePortAddrs[0].String():
			s = "<nodePort>"
		case nodePortAddrs[1].String():
			s = "<nodePort>"
		default:
			if customIPString != nil {
				s = customIPString(addr)
			} else {
				s = addr.String()
			}
			if addr.To4() == nil {
				s = "[" + s + "]"
			}
			s = fmt.Sprintf("%s:%d", s, port)
		}
		return
	}

	svcCB := func(svcKey lbmap.ServiceKey, svcValue lbmap.ServiceValue) {
		svcKey = svcKey.ToHost()
		svcValue = svcValue.ToHost()
		addr := svcKey.GetAddress()
		addrS := replaceAddr(addr, svcKey.GetPort())
		if svcKey.GetScope() == loadbalancer.ScopeInternal {
			addrS += "/i"
		}
		out = append(out, fmt.Sprintf("SVC: ID=%s ADDR=%s SLOT=%d BEID=%s COUNT=%d QCOUNT=%d FLAGS=%s",
			sanitizeID(svcValue.GetRevNat(), sanitizeIDs),
			addrS,
			svcKey.GetBackendSlot(),
			sanitizeID(svcValue.GetBackendID(), sanitizeIDs),
			svcValue.GetCount(),
			svcValue.GetQCount(),
			strings.ReplaceAll(
				loadbalancer.ServiceFlags(svcValue.GetFlags()).String(),
				", ", "+"),
		))
	}
	if err := lbmaps.DumpService(svcCB); err != nil {
		panic(err)
	}

	beCB := func(beKey lbmap.BackendKey, beValue lbmap.BackendValue) {
		beValue = beValue.ToHost()
		addr := beValue.GetAddress()
		addrS := replaceAddr(addr, beValue.GetPort())
		stateS, _ := loadbalancer.GetBackendStateFromFlags(beValue.GetFlags()).String()
		out = append(out, fmt.Sprintf("BE: ID=%s ADDR=%s STATE=%s",
			sanitizeID(beKey.GetID(), sanitizeIDs),
			addrS,
			stateS,
		))
	}
	if err := lbmaps.DumpBackend(beCB); err != nil {
		panic(err)
	}

	revCB := func(revKey lbmap.RevNatKey, revValue lbmap.RevNatValue) {
		revKey = revKey.ToHost()
		revValue = revValue.ToHost()

		var addr string

		switch v := revValue.(type) {
		case *lbmap.RevNat4Value:
			addr = replaceAddr(v.Address.IP(), v.Port)

		case *lbmap.RevNat6Value:
			addr = replaceAddr(v.Address.IP(), v.Port)
		}

		out = append(out, fmt.Sprintf("REV: ID=%s ADDR=%s",
			sanitizeID(revKey.GetKey(), sanitizeIDs),
			addr,
		))
	}
	if err := lbmaps.DumpRevNat(revCB); err != nil {
		panic(err)
	}

	affCB := func(affKey *lbmap.AffinityMatchKey, _ *lbmap.AffinityMatchValue) {
		affKey = affKey.ToHost()
		out = append(out, fmt.Sprintf("AFF: ID=%s BEID=%d",
			sanitizeID(affKey.RevNATID, sanitizeIDs),
			affKey.BackendID,
		))
	}

	if err := lbmaps.DumpAffinityMatch(affCB); err != nil {
		panic(err)
	}

	srcRangeCB := func(key lbmap.SourceRangeKey, _ *lbmap.SourceRangeValue) {
		key = key.ToHost()
		out = append(out, fmt.Sprintf("SRCRANGE: ID=%s CIDR=%s",
			sanitizeID(key.GetRevNATID(), sanitizeIDs),
			key.GetCIDR(),
		))
	}
	if err := lbmaps.DumpSourceRange(srcRangeCB); err != nil {
		panic(err)
	}

	sort.Strings(out)
	return
}

// sanitizeTables clears non-deterministic data in the table output such as timestamps.
func sanitizeTables(dump []byte) []byte {
	r := regexp.MustCompile(`\([^\)]* ago\)`)
	return r.ReplaceAll(dump, []byte("(??? ago)"))
}

func readFileFromDir(file, dir string) func() ([]byte, error) {
	return func() ([]byte, error) {
		return os.ReadFile(path.Join(dir, file))
	}
}

func writeToDir(testDataPath string) func(string, []byte, fs.FileMode) {
	return func(name string, data []byte, perm fs.FileMode) {
		os.WriteFile(path.Join(testDataPath, name), data, 0644)
	}
}

func CheckTables(db *statedb.DB, writer *Writer, svcs []*slim_corev1.Service, epSlices []*k8s.Endpoints) error {
	txn := db.ReadTxn()
	var err error

	{
		if servicesNo := writer.Services().NumObjects(txn); servicesNo != len(svcs) {
			err = errors.Join(err, fmt.Errorf("Incorrect number of services, got %d, want %d", servicesNo, len(svcs)))
		} else {
			i := 0
			for svc := range writer.Services().All(txn) {
				want := svcs[i]
				if svc.Name.Namespace != want.Namespace {
					err = errors.Join(err, fmt.Errorf("Incorrect namespace for service #%06d, got %q, want %q", i, svc.Name.Namespace, want.Namespace))
				}
				if svc.Name.Name != want.Name {
					err = errors.Join(err, fmt.Errorf("Incorrect name for service #%06d, got %q, want %q", i, svc.Name.Name, want.Name))
				}
				if svc.Source != "k8s" {
					err = errors.Join(err, fmt.Errorf("Incorrect source for service #%06d, got %q, want %q", i, svc.Source, "k8s"))
				}
				if svc.ExtTrafficPolicy != loadbalancer.SVCTrafficPolicyCluster {
					err = errors.Join(err, fmt.Errorf("Incorrect external traffic policy for service #%06d, got %q, want %q", i, svc.ExtTrafficPolicy, loadbalancer.SVCTrafficPolicyCluster))
				}
				if svc.IntTrafficPolicy != loadbalancer.SVCTrafficPolicyCluster {
					err = errors.Join(err, fmt.Errorf("Incorrect internal traffic policy for service #%06d, got %q, want %q", i, svc.IntTrafficPolicy, loadbalancer.SVCTrafficPolicyCluster))
				}

				i++
			}
		}
	}

	{
		if frontendsNo := writer.Frontends().NumObjects(txn); frontendsNo != len(svcs) {
			err = errors.Join(err, fmt.Errorf("Incorrect number of frontends, got %d, want %d", frontendsNo, len(svcs)))
		} else {
			i := 0
			for fe := range writer.Frontends().All(txn) {
				want := svcs[i]
				if fe.ServiceName.Namespace != want.Namespace {
					err = errors.Join(err, fmt.Errorf("Incorrect namespace for frontend #%06d, got %q, want %q", i, fe.ServiceName.Namespace, want.Namespace))
				}
				if fe.ServiceName.Name != want.Name {
					err = errors.Join(err, fmt.Errorf("Incorrect name for frontend #%06d, got %q, want %q", i, fe.ServiceName.Name, want.Name))
				}
				wantIP, _ := netip.ParseAddr(want.Spec.ClusterIP)
				if fe.Address.AddrCluster.Addr() != wantIP {
					err = errors.Join(err, fmt.Errorf("Incorrect address for frontend #%06d, got %v, want %v", i, fe.Address.AddrCluster.Addr(), wantIP))
				}
				if fe.Type != loadbalancer.SVCType(want.Spec.Type) {
					err = errors.Join(err, fmt.Errorf("Incorrect service type for frontend #%06d, got %v, want %v", i, fe.Type, loadbalancer.SVCType(want.Spec.Type)))
				}
				if fe.PortName != loadbalancer.FEPortName(want.Spec.Ports[0].Name) {
					err = errors.Join(err, fmt.Errorf("Incorrect port name for frontend #%06d, got %v, want %v", i, fe.PortName, loadbalancer.FEPortName(want.Spec.Ports[0].Name)))
				}
				if fe.Status.Kind != "Done" {
					err = errors.Join(err, fmt.Errorf("Incorrect status for frontend #%06d, got %v, want %v", i, fe.Status.Kind, "Done"))
				}
				for wantAddr := range epSlices[i].Backends { // There is only one element in this map.
					if fe.Backends[0].AddrCluster != wantAddr {
						err = errors.Join(err, fmt.Errorf("Incorrect backend address for frontend #%06d, got %v, want %v", i, fe.Backends[0].AddrCluster, wantAddr))
					}
				}

				i++
			}
		}
	}

	{
		if backendsNo := writer.Backends().NumObjects(txn); backendsNo != len(epSlices) {
			err = errors.Join(err, fmt.Errorf("Incorrect number of backends, got %d, want %d", backendsNo, len(epSlices)))
		} else {
			i := 0
			for be := range writer.Backends().All(txn) {
				want := epSlices[i]
				for wantAddr, wantBe := range want.Backends { // There is only one element in this map.
					if be.AddrCluster != wantAddr {
						err = errors.Join(err, fmt.Errorf("Incorrect address for backend #%06d, got %v, want %v", i, be.AddrCluster, wantAddr))
					}
					for _, wantPort := range wantBe.Ports { // There is only one element in this map.
						if be.Port != wantPort.Port {
							err = errors.Join(err, fmt.Errorf("Incorrect port for backend #%06d, got %v, want %v", i, be.Port, wantPort.Port))
						}
						if be.Protocol != wantPort.Protocol {
							err = errors.Join(err, fmt.Errorf("Incorrect protocol for backend #%06d, got %v, want %v", i, be.Protocol, wantPort.Protocol))
						}
					}
					if be.NodeName != wantBe.NodeName {
						err = errors.Join(err, fmt.Errorf("Incorrect node name for backend #%06d, got %v, want %v", i, be.NodeName, wantBe.NodeName))
					}
				}
				if be.Instances.Len() != 1 {
					err = errors.Join(err, fmt.Errorf("Incorrect instances count for backend #%06d, got %v, want %v", i, be.Instances.Len(), 1))
				} else {
					for svcName, instance := range be.Instances.All() { // There should
						if svcName.Name != svcs[i].Name {
							err = errors.Join(err, fmt.Errorf("Incorrect service name for backend #%06d, got %v, want %v", i, svcName.Name, svcs[i].Name))
						}
						if state, tmpErr := instance.State.String(); tmpErr != nil || state != "active" {
							err = errors.Join(err, fmt.Errorf("Incorrect state for backend #%06d, got %q, want %q", i, state, "active"))
						}
						if instance.PortName != svcs[i].Spec.Ports[0].Name {
							err = errors.Join(err, fmt.Errorf("Incorrect instance port name for backend #%06d, got %q, want %q", i, instance.PortName, svcs[i].Spec.Ports[0].Name))
						}
					}
				}

				i++
			}
		}
	}

	return err
}

func FastCheckTables(db *statedb.DB, writer *Writer, expectedFrontends int, lastPendingRevision statedb.Revision) (reconciled bool, nextRevision statedb.Revision) {
	txn := db.ReadTxn()
	if writer.Frontends().NumObjects(txn) < expectedFrontends {
		return false, 0
	}
	var rev uint64
	var fe *Frontend
	for fe, rev = range writer.Frontends().LowerBound(txn, statedb.ByRevision[*Frontend](lastPendingRevision)) {
		if fe.Status.Kind != reconciler.StatusKindDone {
			return false, rev
		}
	}
	return true, rev // Here, it is the last reconciled revision rather than the first non-reconciled revision.
}

func FastCheckEmptyTables(db *statedb.DB, writer *Writer, bo *BPFOps) bool {
	txn := db.ReadTxn()
	if writer.Frontends().NumObjects(txn) > 0 || writer.Backends().NumObjects(txn) > 0 || writer.Services().NumObjects(txn) > 0 {
		return false
	}
	if len(bo.backendReferences) > 0 || len(bo.backendStates) > 0 || len(bo.nodePortAddrs) > 0 || len(bo.serviceIDAlloc.entities) > 0 || len(bo.backendIDAlloc.entities) > 0 {
		return false
	}
	return true
}

func checkTablesAndMaps(db *statedb.DB, writer *Writer, maps LBMaps, expectedTablesF func() ([]byte, error), expectedMapsF func() ([]byte, error), writeData func(string, []byte, fs.FileMode), customIPString func(net.IP) string) bool {
	allDone := true
	count := 0
	for fe := range writer.Frontends().All(db.ReadTxn()) {
		if fe.Status.Kind != reconciler.StatusKindDone {
			allDone = false
		}
		count++
	}
	if count == 0 || !allDone {
		return false
	}

	var tableBuf bytes.Buffer
	writer.DebugDump(db.ReadTxn(), &tableBuf)
	actualTables := tableBuf.Bytes()

	var expectedTables []byte
	if expectedData, err := expectedTablesF(); err == nil {
		expectedTables = expectedData
	}
	actualTables = sanitizeTables(actualTables)
	expectedTables = sanitizeTables(expectedTables)

	writeData("actual.tables", actualTables, 0644)

	var expectedMaps []MapDump
	if expectedData, err := expectedMapsF(); err == nil {
		expectedMaps = strings.Split(strings.TrimSpace(string(expectedData)), "\n")
	}
	actualMaps := DumpLBMaps(maps, frontendAddrs[0], true, customIPString)

	writeData(
		"actual.maps",
		[]byte(strings.Join(actualMaps, "\n")+"\n"),
		0644,
	)
	return bytes.Equal(actualTables, expectedTables) && slices.Equal(expectedMaps, actualMaps)
}

func logDiff(t *testing.T, fileA, fileB string) {
	t.Helper()

	contentsA, err := os.ReadFile(fileA)
	require.NoError(t, err)
	contentsB, _ := os.ReadFile(fileB)

	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(contentsA)),
		B:        difflib.SplitLines(string(contentsB)),
		FromFile: fileA,
		ToFile:   fileB,
		Context:  2,
	}
	text, _ := difflib.GetUnifiedDiffString(diff)
	if len(text) > 0 {
		t.Logf("\n%s", text)
	}
}

func TestHive(maps LBMaps,
	services chan resource.Event[*slim_corev1.Service],
	pods chan resource.Event[*slim_corev1.Pod],
	endpoints chan resource.Event[*k8s.Endpoints],
	failureProbability float32,
	writer **Writer,
	db **statedb.DB,
	bo **BPFOps,
) *hive.Hive {
	extConfig := externalConfig{
		ExternalClusterIP:     false,
		EnableSessionAffinity: true,
		NodePortMin:           option.NodePortMinDefault,
		NodePortMax:           option.NodePortMaxDefault,
	}

	return hive.New(
		cell.Module(
			"loadbalancer-test",
			"Test module",

			cell.Provide(
				func() Config {
					return Config{
						EnableExperimentalLB: true,
						RetryBackoffMin:      time.Millisecond,
						RetryBackoffMax:      time.Millisecond,
					}
				},
				func() externalConfig { return extConfig },
			),

			cell.Provide(func() streamsOut {
				return streamsOut{
					ServicesStream:  stream.FromChannel(services),
					EndpointsStream: stream.FromChannel(endpoints),
					PodsStream:      stream.FromChannel(pods),
				}
			}),

			cell.Provide(
				func(lc cell.Lifecycle) LBMaps {
					if rm, ok := maps.(*BPFLBMaps); ok {
						lc.Append(rm)
					}
					return maps
					/*
						return &FaultyLBMaps{
							impl:               maps,
							failureProbability: failureProbability,
						}*/
				},
			),

			cell.Invoke(func(db_ *statedb.DB, w *Writer, bo_ *BPFOps) {
				*db = db_
				*writer = w
				*bo = bo_
			}),

			// Provides [Writer] API and the load-balancing tables.
			TablesCell,

			// Reflects Kubernetes services and endpoints to the load-balancing tables
			// using the [Writer].
			ReflectorCell,

			// Reconcile tables to BPF maps
			ReconcilerCell,

			cell.Provide(
				tables.NewNodeAddressTable,
				statedb.RWTable[tables.NodeAddress].ToTable,
			),
			cell.Invoke(func(db *statedb.DB, nodeAddrs statedb.RWTable[tables.NodeAddress]) {
				db.RegisterTable(nodeAddrs)
				txn := db.WriteTxn(nodeAddrs)

				for _, addr := range nodePortAddrs {
					nodeAddrs.Insert(
						txn,
						tables.NodeAddress{
							Addr:       addr,
							NodePort:   true,
							Primary:    true,
							DeviceName: "eth0",
						},
					)
					nodeAddrs.Insert(
						txn,
						tables.NodeAddress{
							Addr:       addr,
							NodePort:   true,
							Primary:    true,
							DeviceName: "eth0",
						},
					)
				}
				txn.Commit()

			}),
		),
	)
}
