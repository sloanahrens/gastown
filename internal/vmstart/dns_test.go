package vmstart

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

const testResolvConf = "# managed by lima\nnameserver 192.168.5.3\nsearch lima.local\n"

// TestVMStartDNSCheck: a failed lookup returns the parsed nameserver with
// Answers false, and a lookup that exited 0 returns Answers true.
func TestVMStartDNSCheck(t *testing.T) {
	t.Parallel()
	wantArgv := []string{"colima", "ssh", "-p", "gt-control", "--", "sh", "-c",
		"cat /etc/resolv.conf; timeout 5 getent hosts github.com"}
	cases := map[string]struct {
		out    string
		runErr error
		want   DNSState
	}{
		"a failing lookup": {
			out:    testResolvConf,
			runErr: errors.New("exit status 1"),
			want: DNSState{
				ResolvConf:  testResolvConf,
				Nameservers: []string{"192.168.5.3"},
				Answers:     false,
			},
		},
		"a passing lookup": {
			out: testResolvConf + "140.82.113.4  github.com\n",
			want: DNSState{
				ResolvConf:  testResolvConf + "140.82.113.4  github.com\n",
				Nameservers: []string{"192.168.5.3"},
				Answers:     true,
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var calls [][]string
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string{name}, args...))
				return []byte(tc.out), tc.runErr
			}
			got, err := CheckDNS(context.Background(), run, "gt-control")
			if err != nil {
				t.Fatalf("CheckDNS: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("state = %+v, want %+v", got, tc.want)
			}
			if len(calls) != 1 || !reflect.DeepEqual(calls[0], wantArgv) {
				t.Fatalf("calls = %q, want one %q", calls, wantArgv)
			}
		})
	}
}

// TestVMStartDNSReadError: a command that printed nothing tells us nothing about
// the resolver, so it is an error rather than Answers false.
func TestVMStartDNSReadError(t *testing.T) {
	t.Parallel()
	run := func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("ssh: connect: connection refused")
	}
	if _, err := CheckDNS(context.Background(), run, "gt-sbx"); err == nil {
		t.Fatal("CheckDNS accepted a read that printed nothing")
	}
}

// TestVMStartDNSProfile: a profile name that is not one of the two VMs is
// refused before any call is made.
func TestVMStartDNSProfile(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{"", "gt-sbx ", "gt-control-2", "GT-SBX"} {
		t.Run(profile, func(t *testing.T) {
			calls := 0
			run := func(context.Context, string, ...string) ([]byte, error) {
				calls++
				return nil, nil
			}
			if _, err := CheckDNS(context.Background(), run, profile); err == nil {
				t.Fatalf("CheckDNS accepted profile %q", profile)
			}
			if calls != 0 {
				t.Fatalf("CheckDNS made %d calls for refused profile %q", calls, profile)
			}
		})
	}
}
