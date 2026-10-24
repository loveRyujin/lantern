//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func signCommand(ctx context.Context, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	b, e := exec.CommandContext(c, "/usr/bin/codesign", args...).CombinedOutput()
	return string(b), e
}

func (x *experiment) prepareFonts() Execution {
	c, e := x.task("fontcache", false, x.exe)
	if e != nil {
		return Execution{Mode: "fontcache", Error: e.Error()}
	}
	defer x.cleanTasks(c.Root)
	run := runFixed(x.ctx, x.exe, c, x.monitor)
	if !goodRun(run) {
		return run
	}
	entries, e := os.ReadDir(filepath.Join(c.Tmp, "mpl"))
	if e != nil {
		run.Error = e.Error()
		return run
	}
	x.fontSeeds = map[string][]byte{}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "fontlist-v") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		p := filepath.Join(c.Tmp, "mpl", entry.Name())
		b, e := os.ReadFile(p)
		if e != nil || len(b) > 1<<20 {
			run.Error = "invalid bounded fontcache"
			return run
		}
		x.fontSeeds[entry.Name()] = b
	}
	if len(x.fontSeeds) == 0 {
		run.Error = "no bundled fontcache generated"
	}
	return run
}

func signedFSDenied(run Execution) bool {
	if !goodRun(run) {
		return false
	}
	for _, name := range []string{"host_read", "database_read", "input_write", "host_write", "runtime_write", "symlink_read", "sibling_read"} {
		if !opDenied(run, name) {
			return false
		}
	}
	for _, name := range []string{"input_read", "runtime_read", "out_write", "tmp_write"} {
		if !opSuccess(run, name) {
			return false
		}
	}
	for _, o := range run.Observations {
		if strings.HasPrefix(o.Operation, "actual_target_") && o.Success {
			return false
		}
	}
	return true
}
func signedNetworkDenied(run Execution) bool {
	return goodRun(run) && opDenied(run, "tcp_connect") && opDenied(run, "udp_exchange") && opDenied(run, "unix_connect")
}

func (x *experiment) signedStage() {
	s := stageStart("signed-path", []string{"Ad-hoc signed actual .app executable/helper and bundled policy/payload are verified; through that path real pandas/Agg PNG and filesystem/network isolation assertions repeat."}, []string{"App Sandbox compatibility is not concluded from an entitlement-disabled baseline; Gatekeeper distribution is not tested."})
	close, e := x.networkFixtures()
	if e != nil {
		s.check("network_denied", false, e.Error())
		x.report.stage(s)
		return
	}
	defer close()
	bundle := filepath.Join(x.out, "signed", "LanternProbe.app")
	macos := filepath.Join(bundle, "Contents", "MacOS")
	resources := filepath.Join(bundle, "Contents", "Resources")
	helper := filepath.Join(macos, "lantern-darwin-probe")
	for _, dir := range []string{macos, resources} {
		if e = os.MkdirAll(dir, 0700); e != nil {
			s.check("bundle_signed", false, e.Error())
			x.report.stage(s)
			return
		}
	}
	b, e := os.ReadFile(x.exe)
	if e != nil {
		s.check("bundle_signed", false, e.Error())
		x.report.stage(s)
		return
	}
	if e = os.WriteFile(helper, b, 0700); e != nil {
		s.check("bundle_signed", false, e.Error())
		x.report.stage(s)
		return
	}
	info := `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>CFBundleExecutable</key><string>lantern-darwin-probe</string><key>CFBundleIdentifier</key><string>dev.lantern.darwin-spike</string><key>CFBundleName</key><string>LanternProbe</string><key>CFBundlePackageType</key><string>APPL</string><key>CFBundleVersion</key><string>1</string></dict></plist>`
	if e = os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), []byte(info), 0600); e != nil {
		s.check("bundle_signed", false, e.Error())
		x.report.stage(s)
		return
	}
	script := filepath.Join(resources, "payload.py")
	if e = os.WriteFile(script, pythonPayload, 0600); e != nil {
		s.check("bundle_signed", false, e.Error())
		x.report.stage(s)
		return
	}
	configs := map[string]taskConfig{}
	roots := []string{}
	policyBytes := []byte{}
	for _, mode := range []string{"benign", "filesystem", "network"} {
		c, e := x.task(mode, true, helper)
		if e != nil {
			s.check("bundle_signed", false, e.Error())
			x.report.stage(s)
			return
		}
		roots = append(roots, c.Root)
		c.Script = script
		c.Policy = filepath.Join(resources, mode+".sb")
		policy := []byte(makePolicy(x.plan, c, helper))
		if e = os.WriteFile(c.Policy, policy, 0600); e != nil {
			s.check("bundle_signed", false, e.Error())
			x.report.stage(s)
			return
		}
		policyBytes = append(policyBytes, []byte(mode+"\n")...)
		policyBytes = append(policyBytes, policy...)
		configs[mode] = c
	}
	x.report.Fingerprints["signed_policy_sha256"] = digest(policyBytes)
	// Build the whole bundle first, sign nested executable then outer bundle;
	// never mutate bundled resources after signing. --deep is verification only.
	helperSign, he := signCommand(x.ctx, "--force", "--sign", "-", helper)
	bundleSign, be := signCommand(x.ctx, "--force", "--sign", "-", bundle)
	verify, ve := signCommand(x.ctx, "--verify", "--deep", "--strict", "--verbose=2", bundle)
	entitlements, ee := signCommand(x.ctx, "--display", "--entitlements", ":-", bundle)
	identity, ie := signCommand(x.ctx, "--display", "--verbose=4", bundle)
	s.check("bundle_signed", he == nil && be == nil, "helper and complete .app signed individually, inside out")
	s.check("signature_verified", ve == nil, "codesign --verify --deep --strict")
	s.check("entitlements_recorded", ee == nil && ie == nil && !strings.Contains(entitlements, "com.apple.security.app-sandbox"), "baseline has no App Sandbox entitlement; ad-hoc/hardened flags recorded verbatim")
	s.Details = map[string]interface{}{"bundle": bundle, "helper": helper, "payload": script, "policies": resources, "helper_sign": helperSign, "bundle_sign": bundleSign, "verify": verify, "entitlements": entitlements, "identity": identity, "app_sandbox_requested": false, "gatekeeper_tested": false}
	_ = writeJSON(filepath.Join(x.out, "signed", "signature.json"), s.Details)
	if he != nil || be != nil || ve != nil || ee != nil || ie != nil {
		s.check("bundle_path_executed", false, "signature prerequisites failed; no fallback to unsigned path")
		s.check("benign_png", false, "not executed")
		s.check("fs_denied", false, "not executed")
		s.check("network_denied", false, "not executed")
		s.check("waited", false, "not executed")
		s.check("temp_clean", x.cleanTasks(roots...), "unused task roots removed")
		x.report.stage(s)
		return
	}
	benign := runFixed(x.ctx, helper, configs["benign"], x.monitor)
	s.Runs = append(s.Runs, benign)
	// Repeat host positive controls through the signed helper itself, restore
	// owned sentinels, then execute the signed policy denial paths.
	hostFS := configs["filesystem"]
	hostFS.Sandboxed = false
	host := runFixed(x.ctx, helper, hostFS, x.monitor)
	s.Runs = append(s.Runs, host)
	for _, p := range []string{hostFS.Input, hostFS.Outside, hostFS.RuntimeSentinel} {
		_ = os.WriteFile(p, []byte(sentinel), 0600)
	}
	fs := runFixed(x.ctx, helper, configs["filesystem"], x.monitor)
	s.Runs = append(s.Runs, fs)
	hostNet := configs["network"]
	hostNet.Sandboxed = false
	netControl := runFixed(x.ctx, helper, hostNet, x.monitor)
	s.Runs = append(s.Runs, netControl)
	network := runFixed(x.ctx, helper, configs["network"], x.monitor)
	s.Runs = append(s.Runs, network)
	s.check("bundle_path_executed", goodRun(benign) && fs.Entered && network.Entered, "all runs launch Contents/MacOS/lantern-darwin-probe and bundled fixed payload/policies")
	pngPath := filepath.Join(configs["benign"].Out, "plot.png")
	validPNG := pngValid(pngPath)
	if validPNG {
		data, e := os.ReadFile(pngPath)
		if e == nil {
			_ = os.WriteFile(filepath.Join(x.out, "signed", "plot.png"), data, 0600)
		}
	}
	s.check("benign_png", goodRun(benign) && opSuccess(benign, "pandas_agg") && validPNG, "real locked pandas computes sum and Agg emits fully decoded bounded PNG")
	fsControl := goodRun(host)
	for _, name := range []string{"input_read", "host_read", "database_read", "runtime_read", "input_write", "host_write", "runtime_write", "symlink_read", "sibling_read"} {
		fsControl = fsControl && opSuccess(host, name)
	}
	s.check("fs_denied", fsControl && signedFSDenied(fs), "same signed helper has successful host controls; policy content and write denials repeat")
	s.check("network_denied", goodRun(netControl) && opSuccess(netControl, "tcp_connect") && opSuccess(netControl, "udp_exchange") && opSuccess(netControl, "unix_connect") && signedNetworkDenied(network), "same signed helper has controlled host roundtrips and isolated EPERM/EACCES")
	s.check("waited", allWaited(s.Runs), "every signed helper direct child Waited")
	s.check("temp_clean", x.cleanTasks(roots...), "execution task roots removed; signed evidence bundle retained")
	s.Limitations = []string{fmt.Sprintf("Signature validity is tested on this runner only; no Developer ID/notarization/clean-machine Gatekeeper claim. Verification output: %s", strings.TrimSpace(verify))}
	x.report.stage(s)
}
