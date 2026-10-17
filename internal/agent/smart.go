package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A disk's own page (Storage, then a disk): every SMART attribute, the
// self-test log and what the disk says it is, read on demand with
// smartctl -x. Like the poller it doesn't wake a sleeping disk, unless asked.

// SMARTDetail is everything a disk's page shows.
type SMARTDetail struct {
	Name         string      `json:"name"`
	Label        string      `json:"label"`
	Asleep       bool        `json:"asleep,omitempty"` // spun down, so nothing was read
	Read         time.Time   `json:"read"`
	Passed       *bool       `json:"passed,omitempty"` // the disk's own verdict; nil: unknown
	Protocol     string      `json:"protocol,omitempty"`
	Family       string      `json:"family,omitempty"`
	Model        string      `json:"model,omitempty"`
	Serial       string      `json:"serial,omitempty"`
	WWN          string      `json:"wwn,omitempty"`
	Firmware     string      `json:"firmware,omitempty"`
	Capacity     uint64      `json:"capacity,omitempty"`
	LogicalBlock int         `json:"logicalBlock,omitempty"`
	PhysBlock    int         `json:"physicalBlock,omitempty"`
	RPM          *int        `json:"rpm,omitempty"` // 0: solid state
	FormFactor   string      `json:"formFactor,omitempty"`
	Interface    string      `json:"interface,omitempty"` // SATA 3.3, 6.0 Gb/s (current: 6.0 Gb/s)
	Standard     string      `json:"standard,omitempty"`  // ATA or NVMe version
	InDatabase   bool        `json:"inDatabase,omitempty"`
	Temp         *int        `json:"temp,omitempty"`
	PowerOnHours *int        `json:"powerOnHours,omitempty"`
	PowerCycles  *int64      `json:"powerCycles,omitempty"`
	Attributes   []SMARTAttr `json:"attributes"` // ATA
	NVMe         *NVMeHealth `json:"nvme,omitempty"`
	SelfTests    []SelfTest  `json:"selfTests"`
	SelfTestNow  string      `json:"selfTestNow,omitempty"` // one running now
	ErrorCount   int         `json:"errorCount"`            // entries in an ATA disk's error log
	Messages     []string    `json:"messages,omitempty"`    // smartctl couldn't read something that matters
	Notes        []string    `json:"notes,omitempty"`       // optional logs the disk doesn't offer
	SelfTestNote string      `json:"selfTestNote,omitempty"`
}

// reOptionalLog matches smartctl's messages about logs a disk may not keep:
// the error and self-test logs, SCT data, device statistics and the like.
var reOptionalLog = regexp.MustCompile(`(?i)\blog\b|\bSCT\b|statistics|phy event`)

// SMARTAttr is one row of an ATA disk's attribute table.
type SMARTAttr struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Flags      int    `json:"flags"`
	Value      int    `json:"value"`
	Worst      int    `json:"worst"`
	Threshold  int    `json:"threshold"`
	PreFail    bool   `json:"preFail"`              // below the threshold, the disk is failing
	Online     bool   `json:"online"`               // updated always, not only by offline scans
	WhenFailed string `json:"whenFailed,omitempty"` // "now" or "past"
	Raw        int64  `json:"raw"`
	RawText    string `json:"rawText"`
}

// NVMeHealth is an NVMe disk's health log (its counterpart of attributes).
type NVMeHealth struct {
	CriticalWarning     int    `json:"criticalWarning"`
	Temp                int    `json:"temp"`
	AvailableSpare      int    `json:"availableSpare"`
	SpareThreshold      int    `json:"spareThreshold"`
	PercentageUsed      int    `json:"percentageUsed"`
	DataUnitsRead       uint64 `json:"dataUnitsRead"` // of 512,000 bytes
	DataUnitsWritten    uint64 `json:"dataUnitsWritten"`
	HostReads           uint64 `json:"hostReads"`
	HostWrites          uint64 `json:"hostWrites"`
	BusyMinutes         uint64 `json:"busyMinutes"`
	PowerCycles         uint64 `json:"powerCycles"`
	PowerOnHours        uint64 `json:"powerOnHours"`
	UnsafeShutdowns     uint64 `json:"unsafeShutdowns"`
	MediaErrors         uint64 `json:"mediaErrors"`
	ErrorLogEntries     uint64 `json:"errorLogEntries"`
	WarningTempMinutes  uint64 `json:"warningTempMinutes"`
	CriticalTempMinutes uint64 `json:"criticalTempMinutes"`
}

// SelfTest is one entry of the self-test log, newest first.
type SelfTest struct {
	Type   string `json:"type"` // Short offline, Extended offline…
	Status string `json:"status"`
	Passed *bool  `json:"passed,omitempty"` // nil: aborted, interrupted or still running
	Hours  int    `json:"hours"`            // the disk's power-on hours when it ran
	LBA    *int64 `json:"lba,omitempty"`    // the first error
}

type valueString struct {
	Value  int    `json:"value"`
	String string `json:"string"`
}

type smartDetailJSON struct {
	Smartctl struct {
		Messages []struct {
			String   string `json:"string"`
			Severity string `json:"severity"`
		} `json:"messages"`
	} `json:"smartctl"`
	Device struct {
		Protocol string `json:"protocol"`
	} `json:"device"`
	ModelFamily string `json:"model_family"`
	ModelName   string `json:"model_name"`
	Serial      string `json:"serial_number"`
	WWN         *struct {
		NAA uint64 `json:"naa"`
		OUI uint64 `json:"oui"`
		ID  uint64 `json:"id"`
	} `json:"wwn"`
	Firmware     string `json:"firmware_version"`
	UserCapacity struct {
		Bytes uint64 `json:"bytes"`
	} `json:"user_capacity"`
	NVMeCapacity uint64 `json:"nvme_total_capacity"`
	LogicalBlock int    `json:"logical_block_size"`
	PhysBlock    int    `json:"physical_block_size"`
	RotationRate *int   `json:"rotation_rate"`
	FormFactor   struct {
		Name string `json:"name"`
	} `json:"form_factor"`
	InDatabase  bool        `json:"in_smartctl_database"`
	ATAVersion  valueString `json:"ata_version"`
	NVMeVersion valueString `json:"nvme_version"`
	SATAVersion valueString `json:"sata_version"`
	Speed       struct {
		Max     valueString `json:"max"`
		Current valueString `json:"current"`
	} `json:"interface_speed"`
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current int `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours int `json:"hours"`
	} `json:"power_on_time"`
	PowerCycles *int64 `json:"power_cycle_count"`
	ATAData     struct {
		SelfTest struct {
			Status struct {
				Value     int    `json:"value"`
				String    string `json:"string"`
				Remaining *int   `json:"remaining_percent"`
			} `json:"status"`
		} `json:"self_test"`
	} `json:"ata_smart_data"`
	Attributes *struct {
		Table []struct {
			ID         int    `json:"id"`
			Name       string `json:"name"`
			Value      int    `json:"value"`
			Worst      int    `json:"worst"`
			Thresh     int    `json:"thresh"`
			WhenFailed string `json:"when_failed"`
			Flags      struct {
				Value         int  `json:"value"`
				Prefailure    bool `json:"prefailure"`
				UpdatedOnline bool `json:"updated_online"`
			} `json:"flags"`
			Raw struct {
				Value  int64  `json:"value"`
				String string `json:"string"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	ErrorLog struct {
		Extended *struct {
			Count int `json:"count"`
		} `json:"extended"`
		Summary *struct {
			Count int `json:"count"`
		} `json:"summary"`
	} `json:"ata_smart_error_log"`
	SelfTestLog struct {
		Extended *ataSelfTests `json:"extended"`
		Standard *ataSelfTests `json:"standard"`
	} `json:"ata_smart_self_test_log"`
	NVMeHealth *struct {
		CriticalWarning  int    `json:"critical_warning"`
		Temperature      int    `json:"temperature"`
		AvailableSpare   int    `json:"available_spare"`
		SpareThreshold   int    `json:"available_spare_threshold"`
		PercentageUsed   int    `json:"percentage_used"`
		DataUnitsRead    uint64 `json:"data_units_read"`
		DataUnitsWritten uint64 `json:"data_units_written"`
		HostReads        uint64 `json:"host_reads"`
		HostWrites       uint64 `json:"host_writes"`
		BusyTime         uint64 `json:"controller_busy_time"`
		PowerCycles      uint64 `json:"power_cycles"`
		PowerOnHours     uint64 `json:"power_on_hours"`
		UnsafeShutdowns  uint64 `json:"unsafe_shutdowns"`
		MediaErrors      uint64 `json:"media_errors"`
		ErrorLogEntries  uint64 `json:"num_err_log_entries"`
		WarningTempTime  uint64 `json:"warning_temp_time"`
		CriticalCompTime uint64 `json:"critical_comp_time"`
	} `json:"nvme_smart_health_information_log"`
	NVMeSelfTests struct {
		Current valueString `json:"current_self_test_operation"`
		Table   []struct {
			Code   valueString `json:"self_test_code"`
			Result valueString `json:"self_test_result"`
			Hours  int         `json:"power_on_hours"`
			LBA    *int64      `json:"lba"`
		} `json:"table"`
	} `json:"nvme_self_test_log"`
}

type ataSelfTests struct {
	Table []struct {
		Type   valueString `json:"type"`
		Status struct {
			String string `json:"string"`
			Passed *bool  `json:"passed"`
		} `json:"status"`
		Hours int    `json:"lifetime_hours"`
		LBA   *int64 `json:"lba"`
	} `json:"table"`
}

// smartDetail reads one disk. Without wake, a sleeping disk is left asleep.
func (a *Agent) smartDetail(ctx context.Context, d Disk, wake bool) (SMARTDetail, error) {
	args := []string{"-x", "-j"}
	if !wake {
		args = append([]string{"-n", "standby"}, args...)
	}
	out, _ := a.cmd(ctx, 30*time.Second, "smartctl", append(args, filepath.Join("/dev", d.Name))...)
	v, err := parseSMARTDetail([]byte(out))
	v.Name, v.Label = d.Name, d.Label
	return v, err
}

// parseSMARTDetail turns smartctl -x -j output into a SMARTDetail.
func parseSMARTDetail(out []byte) (SMARTDetail, error) {
	v := SMARTDetail{Read: time.Now(), Attributes: []SMARTAttr{}, SelfTests: []SelfTest{}}
	var s smartDetailJSON
	if err := json.Unmarshal(out, &s); err != nil {
		// A field of an unexpected type leaves the rest readable.
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			return v, errors.New("smartctl didn't answer. Is smartmontools installed?")
		}
	}
	for _, m := range s.Smartctl.Messages {
		// "Device is in STANDBY mode, exit(2)", or SLEEP, or STANDBY (OS)…
		u := strings.ToUpper(m.String)
		switch {
		case strings.HasPrefix(u, "DEVICE IS IN ") && (strings.Contains(u, "STANDBY") || strings.Contains(u, "SLEEP")):
			v.Asleep = true
		case m.Severity == "information":
		case strings.Contains(u, "SELF-TEST LOG"):
			// Self-tests are optional (many NVMe disks have none), and -x
			// asks for the log anyway: "Invalid Field in Command".
			v.SelfTestNote = "This disk doesn't keep a self-test log, so it most likely can't run self-tests (smartctl: " + m.String + ")."
		case reOptionalLog.MatchString(m.String):
			v.Notes = append(v.Notes, m.String)
		default:
			v.Messages = append(v.Messages, m.String)
		}
	}
	if v.Asleep && s.SmartStatus == nil && s.ModelName == "" {
		return v, nil
	}
	v.Asleep = false

	v.Protocol, v.Family, v.Model, v.Serial, v.Firmware = s.Device.Protocol, s.ModelFamily, s.ModelName, s.Serial, s.Firmware
	if s.WWN != nil {
		v.WWN = fmt.Sprintf("%x %06x %09x", s.WWN.NAA, s.WWN.OUI, s.WWN.ID)
	}
	v.Capacity = max(s.UserCapacity.Bytes, s.NVMeCapacity)
	v.LogicalBlock, v.PhysBlock, v.RPM, v.FormFactor, v.InDatabase = s.LogicalBlock, s.PhysBlock, s.RotationRate, s.FormFactor.Name, s.InDatabase
	v.Standard = s.ATAVersion.String
	if s.NVMeVersion.String != "" {
		v.Standard = "NVMe " + s.NVMeVersion.String
	}
	var iface []string
	for _, p := range []string{s.SATAVersion.String, s.Speed.Max.String} {
		if p != "" {
			iface = append(iface, p)
		}
	}
	v.Interface = strings.Join(iface, ", ")
	if c := s.Speed.Current.String; c != "" && c != s.Speed.Max.String {
		v.Interface += " (now " + c + ")"
	}
	if s.SmartStatus != nil {
		p := s.SmartStatus.Passed
		v.Passed = &p
	}
	if s.Temperature != nil && s.Temperature.Current > 0 {
		t := s.Temperature.Current
		v.Temp = &t
	}
	if s.PowerOnTime != nil {
		h := s.PowerOnTime.Hours
		v.PowerOnHours = &h
	}
	v.PowerCycles = s.PowerCycles

	if s.Attributes != nil {
		for _, t := range s.Attributes.Table {
			v.Attributes = append(v.Attributes, SMARTAttr{
				ID: t.ID, Name: t.Name, Flags: t.Flags.Value, Value: t.Value, Worst: t.Worst, Threshold: t.Thresh,
				PreFail: t.Flags.Prefailure, Online: t.Flags.UpdatedOnline, WhenFailed: t.WhenFailed,
				Raw: t.Raw.Value, RawText: t.Raw.String,
			})
		}
	}
	if st := s.ATAData.SelfTest.Status; st.Remaining != nil && *st.Remaining > 0 {
		v.SelfTestNow = fmt.Sprintf("%d%% left", *st.Remaining)
	}
	if e := s.ErrorLog.Extended; e != nil {
		v.ErrorCount = e.Count
	} else if e := s.ErrorLog.Summary; e != nil {
		v.ErrorCount = e.Count
	}
	log := s.SelfTestLog.Extended
	if log == nil || len(log.Table) == 0 {
		log = s.SelfTestLog.Standard
	}
	if log != nil {
		for _, t := range log.Table {
			v.SelfTests = append(v.SelfTests, SelfTest{Type: t.Type.String, Status: t.Status.String, Passed: t.Status.Passed, Hours: t.Hours, LBA: t.LBA})
		}
	}

	if h := s.NVMeHealth; h != nil {
		v.NVMe = &NVMeHealth{
			CriticalWarning: h.CriticalWarning, Temp: h.Temperature, AvailableSpare: h.AvailableSpare, SpareThreshold: h.SpareThreshold,
			PercentageUsed: h.PercentageUsed, DataUnitsRead: h.DataUnitsRead, DataUnitsWritten: h.DataUnitsWritten,
			HostReads: h.HostReads, HostWrites: h.HostWrites, BusyMinutes: h.BusyTime, PowerCycles: h.PowerCycles,
			PowerOnHours: h.PowerOnHours, UnsafeShutdowns: h.UnsafeShutdowns, MediaErrors: h.MediaErrors,
			ErrorLogEntries: h.ErrorLogEntries, WarningTempMinutes: h.WarningTempTime, CriticalTempMinutes: h.CriticalCompTime,
		}
		// Not ErrorCount: NVMe disks log harmless entries too (an
		// unsupported command), so the count alone means little.
		if v.PowerCycles == nil {
			c := int64(h.PowerCycles)
			v.PowerCycles = &c
		}
	}
	for _, t := range s.NVMeSelfTests.Table {
		passed := t.Result.Value == 0
		v.SelfTests = append(v.SelfTests, SelfTest{Type: t.Code.String, Status: t.Result.String, Passed: &passed, Hours: t.Hours, LBA: t.LBA})
	}
	if c := s.NVMeSelfTests.Current; c.Value != 0 {
		v.SelfTestNow = c.String
	}
	return v, nil
}

// getSMART serves GET /v1/disks/smart?name=sdc[&wake=1], for a disk the
// poller knows: nothing else is handed to smartctl.
func (a *Agent) getSMART(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	for _, d := range a.disks.snapshot() {
		if d.Name != name {
			continue
		}
		v, err := a.smartDetail(r.Context(), d, r.URL.Query().Get("wake") == "1")
		if err != nil {
			writeErr(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, v)
		return
	}
	writeErr(w, 404, fmt.Sprintf("There's no disk called %q.", name))
}
