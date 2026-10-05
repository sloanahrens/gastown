package townhealth

import (
	"errors"
	"fmt"
	"time"
)

// This file is the landing-infrastructure side of the health line
// (gt-fn9e6.11): whether the Forgejo instance a rig lands through answers,
// and whether its CI has a runner online. Both are what makes a landing wait
// rather than fail — a down instance or an offline runner turns into CI
// silence, which the landing worker answers with retries and backoff on its
// infrastructure path. These fields do not change any of that: they make the
// silence visible and name it before an operator has to read a queue.

// Field names for the landing infrastructure.
const (
	// FieldForgejo is the instance's version endpoint: answered or not.
	FieldForgejo = "forgejo"
	// FieldRunner is whether the rig's repository has a CI runner online.
	FieldRunner = "runner"
)

// InfraRedAfter is how long a landing-infrastructure failure may run before
// its field reads RED; under it the field reads DEGRADED. Five minutes is
// shorter than one landing's gate budget, so an outage is named before the
// first landing it strands, and long enough that a runner restart or a
// dropped request does not (gt-fn9e6.11).
const InfraRedAfter = 5 * time.Minute

// LandingInfra is the landing-infrastructure probe's last observation, made
// by the daemon's landing_infra patrol.
type LandingInfra interface {
	LandingInfra() LandingInfraReport
}

// LandingInfraReport is one observation of every rig that lands through
// Forgejo. A town where no rig does reports Configured false, and the fields
// are absent: the question these probes answer is not asked there.
type LandingInfraReport struct {
	Configured bool
	Rigs       []RigInfra
}

// RigInfra is one rig's landing infrastructure: the Forgejo instance its
// landing path uses and the CI runners its repository has.
type RigInfra struct {
	Rig     string
	Forgejo InfraProbe
	Runner  InfraProbe
}

// InfraProbe is one probe's outcome as the patrol last observed it.
type InfraProbe struct {
	// Unavailable, when set, is why the probe could not be made at all: no
	// base URL configured, no readable token file, no pass run yet. The
	// field reads UNKNOWN with this reason and is never red — a question
	// nobody could ask is not a bad answer (gt-fn9e6.11).
	Unavailable string
	// OK is the probe's answer: the version endpoint returned 200, or the
	// repository has at least one online runner.
	OK bool
	// Since is when the current failing run began; zero when OK or when the
	// probe could not be made.
	Since time.Time
	// Detail is the failure in one phrase ("" when OK).
	Detail string
	// Online is how many of the repository's runners are online, the runner
	// probe's display value.
	Online int
}

// InfraVerdict is the rule the forgejo and runner fields apply, exported so
// the daemon's probe patrol escalates on exactly the verdicts the health line
// shows (gt-fn9e6.11):
//
//	UNKNOWN   the probe could not be made;
//	GREEN     the probe answered healthy;
//	DEGRADED  a failing run younger than InfraRedAfter;
//	RED       a failing run InfraRedAfter or older.
func InfraVerdict(p InfraProbe, now time.Time) Verdict {
	switch {
	case p.Unavailable != "":
		return VerdictUnknown
	case p.OK:
		return Green
	case !p.Since.IsZero() && now.Sub(p.Since) >= InfraRedAfter:
		return Red
	default:
		return Degraded
	}
}

// infra builds the forgejo and runner fields, one pair per rig that lands
// through Forgejo. A town with no such rig adds no field at all.
func infra(in Inputs) []Field {
	if in.LandingInfra == nil {
		return nil
	}
	rep := in.LandingInfra.LandingInfra()
	if !rep.Configured {
		return nil
	}
	fs := make([]Field, 0, 2*len(rep.Rigs))
	for _, r := range rep.Rigs {
		fs = append(fs, infraField(FieldForgejo, r.Rig, r.Forgejo, in.Now),
			infraField(FieldRunner, r.Rig, r.Runner, in.Now))
	}
	return fs
}

// infraField is one probe's field. The tag is RECORDED: the probe is the
// patrol's, made on its own cadence, and this computation reads the record it
// left rather than reaching the instance itself.
func infraField(name, rig string, p InfraProbe, now time.Time) Field {
	switch v := InfraVerdict(p, now); v {
	case VerdictUnknown:
		reason := p.Unavailable
		if reason == "" {
			reason = "the probe has not run yet"
		}
		return unknown(name, rig, "", errors.New(reason))
	case Green:
		value := "ok"
		if name == FieldRunner {
			value = fmt.Sprintf("%d online", p.Online)
		}
		return Field{Name: name, Rig: rig, Tag: Recorded, Verdict: Green, Value: value}
	default:
		f := Field{Name: name, Rig: rig, Tag: Recorded, Verdict: v, Value: infraFailValue(name, infraAge(p, now))}
		f.Detail = p.Detail
		return f
	}
}

// infraAge is how long the failing run has lasted; zero on a probe that
// recorded no start, which the rule then reads as just begun.
func infraAge(p InfraProbe, now time.Time) time.Duration {
	if p.Since.IsZero() {
		return 0
	}
	return now.Sub(p.Since)
}

// infraFailValue says what is wrong and for how long: the duration is what
// tells a reader whether the field is about to turn red.
func infraFailValue(name string, age time.Duration) string {
	if name == FieldRunner {
		return "none online " + Short(age)
	}
	return "down " + Short(age)
}
