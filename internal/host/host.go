// Package host inspects the machine the CLI runs on: can it run the vm
// runtime (KubeVirt needs /dev/kvm), and if not, what to change.
package host

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// Report describes the virtualization support of a Linux host.
type Report struct {
	// KVM is true when /dev/kvm exists.
	KVM bool
	// CPUFlag is "vmx" (Intel VT-x), "svm" (AMD-V) or empty.
	CPUFlag string
	// Hypervisor names the hypervisor this machine runs under ("" on bare
	// metal, "unknown" when one is detected but not recognized).
	Hypervisor string
	// Nested is the nested-virtualization setting of the kvm module
	// ("Y", "N" or "" when the module is not loaded): whether VMs of this
	// machine may themselves run VMs.
	Nested string
}

// VMReady reports whether the vm runtime can run here.
func (r Report) VMReady() bool { return r.KVM }

// Inspect reads the host's state from root, the filesystem root (os.DirFS("/")).
func Inspect(root fs.FS) (Report, error) {
	var r Report
	if _, err := fs.Stat(root, "dev/kvm"); err == nil {
		r.KVM = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return r, fmt.Errorf("checking /dev/kvm: %w", err)
	}

	cpuinfo, err := fs.ReadFile(root, "proc/cpuinfo")
	if err != nil {
		return r, fmt.Errorf("reading /proc/cpuinfo: %w", err)
	}
	flags := cpuFlags(cpuinfo)
	switch {
	case flags["vmx"]:
		r.CPUFlag = "vmx"
	case flags["svm"]:
		r.CPUFlag = "svm"
	}
	if flags["hypervisor"] {
		r.Hypervisor = hypervisor(root)
	}
	for _, module := range []string{"kvm_intel", "kvm_amd"} {
		if v, err := fs.ReadFile(root, "sys/module/"+module+"/parameters/nested"); err == nil {
			r.Nested = normalizeBool(string(v))
		}
	}
	return r, nil
}

// cpuFlags returns the flags of the first CPU.
func cpuFlags(cpuinfo []byte) map[string]bool {
	flags := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(cpuinfo))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if ok && strings.TrimSpace(key) == "flags" {
			for _, f := range strings.Fields(value) {
				flags[f] = true
			}
			break
		}
	}
	return flags
}

// hypervisor recognizes the virtual hardware from its DMI strings.
func hypervisor(root fs.FS) string {
	read := func(name string) string {
		v, _ := fs.ReadFile(root, "sys/class/dmi/id/"+name)
		return strings.ToLower(strings.TrimSpace(string(v)))
	}
	vendor, product, board := read("sys_vendor"), read("product_name"), read("board_vendor")
	all := vendor + " " + product + " " + board
	switch {
	case strings.Contains(all, "proxmox"):
		return "Proxmox"
	case strings.Contains(vendor, "qemu") || strings.Contains(product, "kvm"):
		return "QEMU/KVM (Proxmox, libvirt...)"
	case strings.Contains(vendor, "vmware"):
		return "VMware"
	case strings.Contains(vendor, "microsoft"):
		return "Hyper-V"
	case strings.Contains(vendor, "innotek") || strings.Contains(product, "virtualbox"):
		return "VirtualBox"
	case strings.Contains(vendor, "xen"):
		return "Xen"
	case strings.Contains(vendor, "amazon"), strings.Contains(vendor, "google"):
		return "a cloud provider"
	}
	return "unknown"
}

func normalizeBool(v string) string {
	switch strings.TrimSpace(v) {
	case "Y", "y", "1":
		return "Y"
	case "N", "n", "0":
		return "N"
	}
	return strings.TrimSpace(v)
}

// Advice explains how to get the vm runtime working, or is empty when it
// already can.
func (r Report) Advice() []string {
	if r.VMReady() {
		return nil
	}
	switch {
	case r.Hypervisor != "" && r.CPUFlag == "":
		advice := []string{
			"This machine is a virtual machine (" + r.Hypervisor + ") without nested virtualization: its VMs cannot run VMs.",
		}
		if strings.HasPrefix(r.Hypervisor, "Proxmox") || strings.HasPrefix(r.Hypervisor, "QEMU") {
			advice = append(advice,
				"On Proxmox: set this VM's CPU type to \"host\" (Hardware > Processors), then restart it.",
				"Nested virtualization must also be on in the Proxmox host: `cat /sys/module/kvm_intel/parameters/nested` (or kvm_amd) prints Y;",
				"otherwise add `options kvm_intel nested=1` (or kvm_amd) to /etc/modprobe.d/kvm.conf there and reboot it.")
		} else {
			advice = append(advice, "Enable nested virtualization for this VM in "+r.Hypervisor+", then restart it.")
		}
		return advice
	case r.CPUFlag != "":
		module := map[string]string{"vmx": "kvm_intel", "svm": "kvm_amd"}[r.CPUFlag]
		return []string{
			"The CPU supports virtualization but /dev/kvm is missing: load the kvm module (`sudo modprobe " + module + "`).",
		}
	default:
		return []string{
			"The CPU does not expose virtualization: enable Intel VT-x or AMD-V (SVM) in the firmware (BIOS/UEFI) settings.",
		}
	}
}
