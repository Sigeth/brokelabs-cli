package host

import (
	"strings"
	"testing"
	"testing/fstest"
)

func cpuinfo(flags string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("processor\t: 0\nvendor_id\t: GenuineIntel\nflags\t\t: fpu vme " + flags + "\n\nprocessor\t: 1\nflags\t\t: fpu\n")}
}

func TestInspect(t *testing.T) {
	for name, tc := range map[string]struct {
		files       fstest.MapFS
		want        Report
		adviceMatch string
	}{
		"bare metal with kvm": {
			files: fstest.MapFS{"dev/kvm": {}, "proc/cpuinfo": cpuinfo("vmx ept"), "sys/module/kvm_intel/parameters/nested": {Data: []byte("Y\n")}},
			want:  Report{KVM: true, CPUFlag: "vmx", Nested: "Y"},
		},
		"proxmox vm without nested": {
			files: fstest.MapFS{
				"proc/cpuinfo":                  cpuinfo("hypervisor sse4_2"),
				"sys/class/dmi/id/sys_vendor":   {Data: []byte("QEMU\n")},
				"sys/class/dmi/id/product_name": {Data: []byte("Standard PC (i440FX + PIIX, 1996)\n")},
			},
			want:        Report{Hypervisor: "QEMU/KVM (Proxmox, libvirt...)"},
			adviceMatch: `CPU type to "host"`,
		},
		"vmware vm without nested": {
			files:       fstest.MapFS{"proc/cpuinfo": cpuinfo("hypervisor"), "sys/class/dmi/id/sys_vendor": {Data: []byte("VMware, Inc.\n")}},
			want:        Report{Hypervisor: "VMware"},
			adviceMatch: "nested virtualization for this VM in VMware",
		},
		"nested vm, module not loaded": {
			files:       fstest.MapFS{"proc/cpuinfo": cpuinfo("hypervisor svm"), "sys/class/dmi/id/sys_vendor": {Data: []byte("QEMU")}},
			want:        Report{CPUFlag: "svm", Hypervisor: "QEMU/KVM (Proxmox, libvirt...)"},
			adviceMatch: "modprobe kvm_amd",
		},
		"virtualization off in firmware": {
			files:       fstest.MapFS{"proc/cpuinfo": cpuinfo("sse4_2")},
			want:        Report{},
			adviceMatch: "firmware",
		},
	} {
		got, err := Inspect(tc.files)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: Inspect = %+v, want %+v", name, got, tc.want)
		}
		advice := strings.Join(got.Advice(), "\n")
		if tc.adviceMatch == "" && advice != "" || !strings.Contains(advice, tc.adviceMatch) {
			t.Errorf("%s: advice = %q, want it to mention %q", name, advice, tc.adviceMatch)
		}
	}
}
