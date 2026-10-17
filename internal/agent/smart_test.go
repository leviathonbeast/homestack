package agent

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// Trimmed from smartctl 7.4 -x -j on a SATA hard disk, mid self-test.
const ataSMART = `{
  "smartctl": {"version": [7, 4], "exit_status": 64},
  "device": {"name": "/dev/sde", "type": "sat", "protocol": "ATA"},
  "model_family": "Seagate IronWolf Pro",
  "model_name": "ST16000VN001-2RV103",
  "serial_number": "WRS0FDJN",
  "wwn": {"naa": 5, "oui": 3152, "id": 3508749671},
  "firmware_version": "SC60",
  "user_capacity": {"blocks": 31251759104, "bytes": 16000900661248},
  "logical_block_size": 512,
  "physical_block_size": 4096,
  "rotation_rate": 7200,
  "form_factor": {"ata_value": 2, "name": "3.5 inches"},
  "in_smartctl_database": true,
  "ata_version": {"string": "ACS-4 (minor revision not indicated)", "major_value": 4064},
  "sata_version": {"string": "SATA 3.3", "value": 511},
  "interface_speed": {"max": {"sata_value": 14, "string": "6.0 Gb/s"}, "current": {"sata_value": 2, "string": "3.0 Gb/s"}},
  "smart_status": {"passed": true},
  "ata_smart_data": {"self_test": {"status": {"value": 249, "string": "in progress, 90% remaining", "remaining_percent": 90}}},
  "ata_smart_attributes": {"revision": 10, "table": [
    {"id": 1, "name": "Raw_Read_Error_Rate", "value": 82, "worst": 64, "thresh": 44, "when_failed": "",
     "flags": {"value": 15, "string": "POSR-- ", "prefailure": true, "updated_online": true}, "raw": {"value": 167479336, "string": "167479336"}},
    {"id": 5, "name": "Reallocated_Sector_Ct", "value": 100, "worst": 100, "thresh": 10, "when_failed": "",
     "flags": {"value": 51, "string": "PO--CK ", "prefailure": true, "updated_online": true}, "raw": {"value": 8, "string": "8"}},
    {"id": 198, "name": "Offline_Uncorrectable", "value": 100, "worst": 100, "thresh": 0, "when_failed": "past",
     "flags": {"value": 16, "string": "----C- ", "prefailure": false, "updated_online": false}, "raw": {"value": 0, "string": "0"}}
  ]},
  "power_on_time": {"hours": 10128},
  "power_cycle_count": 51,
  "temperature": {"current": 38},
  "ata_smart_error_log": {"extended": {"revision": 1, "sectors": 5, "count": 2}},
  "ata_smart_self_test_log": {"extended": {"revision": 1, "sectors": 1, "count": 2, "table": [
    {"type": {"value": 1, "string": "Short offline"}, "status": {"value": 0, "string": "Completed without error", "passed": true}, "lifetime_hours": 10100},
    {"type": {"value": 2, "string": "Extended offline"}, "status": {"value": 121, "string": "Completed: read failure", "passed": false}, "lifetime_hours": 9000, "lba": 123456}
  ]}}
}`

// The messages are what smartctl 7.4 says about a Crucial P3, which keeps no
// self-test log, and a disk without an error log.
const nvmeSMART = `{
  "smartctl": {"exit_status": 4, "messages": [
    {"string": "Read Self-test Log failed: Invalid Field in Command (0x2002)", "severity": "error"},
    {"string": "Read Error Information Log failed: Invalid Field in Command (0x2002)", "severity": "error"}]},
  "device": {"name": "/dev/nvme0n1", "type": "nvme", "protocol": "NVMe"},
  "model_name": "CT2000P3PSSD8",
  "serial_number": "2414E8A3D57A",
  "firmware_version": "P9CR40A",
  "nvme_version": {"string": "1.4", "value": 66560},
  "nvme_total_capacity": 2000398934016,
  "smart_status": {"passed": true, "nvme": {"value": 0}},
  "nvme_smart_health_information_log": {"critical_warning": 0, "temperature": 34, "available_spare": 100, "available_spare_threshold": 5,
    "percentage_used": 29, "data_units_read": 12345678, "data_units_written": 87654321, "host_reads": 1, "host_writes": 2,
    "controller_busy_time": 300, "power_cycles": 120, "power_on_hours": 15768, "unsafe_shutdowns": 14, "media_errors": 0,
    "num_err_log_entries": 77, "warning_temp_time": 0, "critical_comp_time": 0},
  "temperature": {"current": 34},
  "power_on_time": {"hours": 15768},
  "nvme_self_test_log": {"current_self_test_operation": {"value": 0, "string": "No self-test in progress"},
    "table": [{"self_test_code": {"value": 1, "string": "Short"}, "self_test_result": {"value": 0, "string": "Completed without error"}, "power_on_hours": 15000}]}
}`

const asleepSMART = `{
  "smartctl": {"messages": [{"string": "Device is in STANDBY mode, exit(2)", "severity": "information"}], "exit_status": 2},
  "device": {"name": "/dev/sde", "protocol": "ATA"}
}`

func TestParseSMARTDetailATA(t *testing.T) {
	v, err := parseSMARTDetail([]byte(ataSMART))
	if err != nil {
		t.Fatal(err)
	}
	if v.Asleep || v.Passed == nil || !*v.Passed || v.Protocol != "ATA" || v.Family != "Seagate IronWolf Pro" || v.Model != "ST16000VN001-2RV103" ||
		v.Serial != "WRS0FDJN" || v.WWN != "5 000c50 0d1234567" || v.Firmware != "SC60" || v.Capacity != 16000900661248 ||
		v.LogicalBlock != 512 || v.PhysBlock != 4096 || v.RPM == nil || *v.RPM != 7200 || v.FormFactor != "3.5 inches" || !v.InDatabase {
		t.Fatalf("identity: %+v", v)
	}
	if v.Interface != "SATA 3.3, 6.0 Gb/s (now 3.0 Gb/s)" || v.Standard != "ACS-4 (minor revision not indicated)" {
		t.Fatalf("interface %q, standard %q", v.Interface, v.Standard)
	}
	if *v.Temp != 38 || *v.PowerOnHours != 10128 || *v.PowerCycles != 51 || v.ErrorCount != 2 || v.SelfTestNow != "90% left" || v.NVMe != nil {
		t.Fatalf("state: %+v", v)
	}
	want := []SMARTAttr{
		{ID: 1, Name: "Raw_Read_Error_Rate", Flags: 15, Value: 82, Worst: 64, Threshold: 44, PreFail: true, Online: true, Raw: 167479336, RawText: "167479336"},
		{ID: 5, Name: "Reallocated_Sector_Ct", Flags: 51, Value: 100, Worst: 100, Threshold: 10, PreFail: true, Online: true, Raw: 8, RawText: "8"},
		{ID: 198, Name: "Offline_Uncorrectable", Flags: 16, Value: 100, Worst: 100, WhenFailed: "past", RawText: "0"},
	}
	if !slices.Equal(v.Attributes, want) {
		t.Fatalf("attributes:\n%+v\nwant\n%+v", v.Attributes, want)
	}
	if len(v.SelfTests) != 2 || v.SelfTests[0].Type != "Short offline" || !*v.SelfTests[0].Passed || v.SelfTests[0].Hours != 10100 || v.SelfTests[0].LBA != nil ||
		*v.SelfTests[1].Passed || v.SelfTests[1].Status != "Completed: read failure" || *v.SelfTests[1].LBA != 123456 {
		t.Fatalf("self-tests: %+v", v.SelfTests)
	}
}

func TestParseSMARTDetailNVMe(t *testing.T) {
	v, err := parseSMARTDetail([]byte(nvmeSMART))
	if err != nil {
		t.Fatal(err)
	}
	if v.Protocol != "NVMe" || v.Standard != "NVMe 1.4" || v.Capacity != 2000398934016 || v.RPM != nil || v.Attributes == nil || len(v.Attributes) != 0 {
		t.Fatalf("identity: %+v", v)
	}
	h := v.NVMe
	if h == nil || h.PercentageUsed != 29 || h.SpareThreshold != 5 || h.DataUnitsWritten != 87654321 || h.UnsafeShutdowns != 14 || h.ErrorLogEntries != 77 {
		t.Fatalf("health: %+v", h)
	}
	// NVMe error log entries are often harmless, so they don't count as errors.
	if v.ErrorCount != 0 || *v.PowerCycles != 120 || v.SelfTestNow != "" {
		t.Fatalf("state: %+v", v)
	}
	if len(v.SelfTests) != 1 || v.SelfTests[0].Type != "Short" || !*v.SelfTests[0].Passed || v.SelfTests[0].Hours != 15000 {
		t.Fatalf("self-tests: %+v", v.SelfTests)
	}
	// Logs the disk doesn't keep are notes, not warnings about its health.
	if v.Messages != nil || !strings.Contains(v.SelfTestNote, "doesn't keep a self-test log") ||
		!slices.Equal(v.Notes, []string{"Read Error Information Log failed: Invalid Field in Command (0x2002)"}) {
		t.Fatalf("messages %q, notes %q, self-test note %q", v.Messages, v.Notes, v.SelfTestNote)
	}
}

func TestParseSMARTDetailOdd(t *testing.T) {
	v, err := parseSMARTDetail([]byte(asleepSMART))
	if err != nil || !v.Asleep || v.Messages != nil || v.Model != "" {
		t.Fatalf("asleep: %+v %v", v, err)
	}
	if _, err := parseSMARTDetail([]byte("sh: smartctl: not found\n")); err == nil {
		t.Fatal("not JSON, but no error")
	}
	// A field of an unexpected type doesn't lose the rest.
	v, err = parseSMARTDetail([]byte(`{"model_name": "X1", "temperature": {"current": "hot"}, "smart_status": {"passed": false},
		"smartctl": {"messages": [{"string": "Read SMART Data failed", "severity": "error"}]}}`))
	if err != nil || v.Model != "X1" || v.Passed == nil || *v.Passed || v.Temp != nil || !slices.Equal(v.Messages, []string{"Read SMART Data failed"}) {
		t.Fatalf("odd field: %+v %v", v, err)
	}
}

func TestGetSMART(t *testing.T) {
	r := &scriptRunner{out: map[string]string{"smartctl -n standby -x -j /dev/sde": asleepSMART, "smartctl -x -j /dev/sde": "!" + ataSMART}}
	a := New(Options{StateDir: t.TempDir(), Runner: r})
	a.disks.disks = []Disk{{Name: "sde", Label: "disk5"}}
	get := func(q string) (int, SMARTDetail) {
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, httptest.NewRequest("GET", "/v1/disks/smart?"+q, nil))
		var v SMARTDetail
		json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v
	}

	// Asleep: left asleep.
	if code, v := get("name=sde"); code != 200 || !v.Asleep || v.Label != "disk5" {
		t.Fatalf("asleep: %d %+v", code, v)
	}
	// Asked to wake it. smartctl's exit status is a bit mask (64: errors
	// logged), so non-zero still has the answer.
	if code, v := get("name=sde&wake=1"); code != 200 || v.Asleep || v.Model != "ST16000VN001-2RV103" || v.Name != "sde" {
		t.Fatalf("woken: %d %+v", code, v)
	}
	// Only disks the poller found go to smartctl.
	for _, q := range []string{"name=sdz", "name=../sde", "name=", "name=-a"} {
		if code, _ := get(q); code != 404 {
			t.Errorf("%s: %d", q, code)
		}
	}
	if want := []string{"smartctl -n standby -x -j /dev/sde", "smartctl -x -j /dev/sde"}; !slices.Equal(r.ran, want) {
		t.Fatalf("ran:\n%s", strings.Join(r.ran, "\n"))
	}
}
