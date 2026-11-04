package tools

import "testing"

func TestRunPythonSchemaReachesPolicyForPrivileges(t *testing.T) {
	reg := &Registry{}
	if err := reg.Register(NewStub(SpecRunPython)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"code":"print(1)"}`,
		`{"code":"print(1)","sql_call_id":"query-1"}`,
		`{"code":"print(1)","network":true}`,
		`{"code":"print(1)","network":"loopback"}`,
		`{"code":"print(1)","network":"127.0.0.1"}`,
		`{"code":"print(1)","network":"::1"}`,
		`{"code":"print(1)","network":false}`,
		`{"code":"print(1)","extra_read_only":["/data/ro"]}`,
		`{"code":"print(1)","limits":{"cpu_ms":20000,"memory_bytes":805306368,"wall_ms":45000,"nproc":1}}`,
		`{"code":"print(1)","limits":{"cpu_ms":30001,"nproc":2}}`,
		`{"code":"print(1)","approved":true,"requires_approval":false}`,
	} {
		if _, err := reg.PrepareRaw("run_python", []byte(raw)); err != nil {
			t.Fatalf("超权限申请没有到达 Policy: %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{"code":"print(1)","inputs":{"secret":"x"}}`,
		`{"code":"print(1)","runtime":{"python":"/bin/sh"}}`,
		`{"code":"print(1)","limits":{"memory":1}}`,
		`{"code":"print(1)","network":{"mode":"loopback"}}`,
		`{"code":"print(1)","sql_call_id":""}`,
		`{"code":null}`,
	} {
		if _, err := reg.PrepareRaw("run_python", []byte(raw)); !ArgumentFailure(err) {
			t.Fatalf("不该进入 Policy 的参数通过了 Schema: %s: %v", raw, err)
		}
	}
}
