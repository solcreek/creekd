package sandbox

import (
	"os/exec"
	"testing"
)

func TestSpecAny(t *testing.T) {
	cases := []struct {
		name string
		s    Spec
		want bool
	}{
		{"zero", Spec{}, false},
		{"pid", Spec{PIDNamespace: true}, true},
		{"uts", Spec{UTSNamespace: true}, true},
		{"ipc", Spec{IPCNamespace: true}, true},
		{"mount", Spec{MountNamespace: true}, true},
		{"chroot", Spec{Chroot: "/tmp/x"}, true},
		{"user", Spec{UserNamespace: true}, true},
		{"nonewprivs", Spec{NoNewPrivs: true}, true},
		{"all", Spec{
			PIDNamespace: true, UTSNamespace: true,
			IPCNamespace: true, MountNamespace: true,
			UserNamespace: true, NoNewPrivs: true,
			Chroot: "/tmp/y",
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.Any(); got != c.want {
				t.Errorf("Any() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestApplyNilCmdRejected(t *testing.T) {
	if err := Apply(nil, Spec{PIDNamespace: true}); err == nil {
		t.Error("expected error for nil cmd")
	}
}

func TestApplyZeroSpecIsNoop(t *testing.T) {
	cmd := exec.Command("true")
	if err := Apply(cmd, Spec{}); err != nil {
		t.Errorf("Apply with zero spec: %v", err)
	}
	if cmd.SysProcAttr != nil {
		t.Errorf("zero spec mutated SysProcAttr: %+v", cmd.SysProcAttr)
	}
}
