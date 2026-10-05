package townhealth

import (
	"errors"
	"fmt"
	"time"
)

// This file is the landing-infrastructure side of the health line
// (gt-fn9e6.11): whether the Forgejo instance a rig lands through answers, and
// whether CI is picking the runs pushed to it up. Both are what makes a
// landing wait rather than fail — a down instance or a stalled run queue turns
// into CI silence, which the landing worker answers with retries and backoff
// on its infrastructure path. These fields do not change any of that: they
// make the silence visible and name it before an operator has to read a queue.

// Field names for the landing infrastructure.
const (
	// FieldForgejo is the instance's version endpoint: answered or not.
	FieldForgejo = "forgejo"
	// FieldRunner is whether the rig's repository is having its runs picked
	// up: the oldest run waiting on a runner.
	FieldRunner = "runner"
)

// InfraRedAfter is how long the Forgejo version probe may fail before its
// field reads RED; under it the field reads DEGRADED. Five minutes is shorter
// than one landing's gate budget, so an outage is named before the first
// landing it strands, and long enough that a dropped request does not
// (gt-fn9e6.11). The runner field judges by run age instead, at the thresholds
// below.
const InfraRedAfter = 5 * time.Minute

// The run pickup thresholds (gt-fn9e6.56). A run nothing has picked up for
// three minutes is late enough to read DEGRADED, and ten minutes means CI is
// not taking work: a probe's cadence is a minute, so ten is well past any
// start-up lag a healthy runner has.
const (
	InfraRunnerDegradedAfter = 3 * time.Minute
	InfraRunnerRedAfter      = 10 * time.Minute
)

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
// landing path uses and the run queue its repository feeds.
type RigInfra struct {
	Rig     string
	Forgejo InfraProbe
	Runner  InfraProbe
}

// InfraProbe is one probe's outcome as the patrol last observed it.
type InfraProbe struct {
	// Unavailable, when set, is why the probe could not be made at all: no
	// base URL configured, no readable token file, an endpoint the token
	// cannot read, no pass run yet. The field reads UNKNOWN with this reason
	// and is never red — a question nobody could ask is not a bad answer
	// (gt-fn9e6.11, gt-fn9e6.56).
	Unavailable string
	// OK is the version probe's answer: the endpoint returned 200. The runner
	// field judges from WaitingSince instead.
	OK bool
	// Since is when the current failing run began; zero when OK or when the
	// probe could not be made. The version field's datum.
	Since time.Time
	// Detail is the version probe's failure in one phrase ("" when OK). The
	// runner field renders its own detail from the run it names.
	Detail string
	// WaitingSince is when the oldest run waiting on a runner was created;
	// zero when nothing is waiting. The runner field's datum, which replaces
	// the online runner count: an instance-level runner appears in no
	// repository listing, so the count read "none" for a runner that was
	// working. An idle runner with an empty queue reads green here until a run
	// waits, which is the limitation this signal accepts (gt-fn9e6.56).
	WaitingSince time.Time
	// WaitingRun is that run's id, named in the field's detail.
	WaitingRun int64
}

// InfraVerdict is the rule the forgejo field applies, exported so the daemon's
// probe patrol escalates on exactly the verdicts the health line shows
// (gt-fn9e6.11):
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

// InfraRunnerVerdict is InfraVerdict for the runner field: the age of the
// oldest run waiting on a runner, at the run pickup thresholds (gt-fn9e6.56).
//
//	UNKNOWN   the run list could not be read;
//	GREEN     nothing waits past InfraRunnerDegradedAfter;
//	DEGRADED  a run has waited InfraRunnerDegradedAfter;
//	RED       a run has waited InfraRunnerRedAfter.
func InfraRunnerVerdict(p InfraProbe, now time.Time) Verdict {
	switch {
	case p.Unavailable != "":
		return VerdictUnknown
	case p.WaitingSince.IsZero():
		return Green
	case now.Sub(p.WaitingSince) >= InfraRunnerRedAfter:
		return Red
	case now.Sub(p.WaitingSince) >= InfraRunnerDegradedAfter:
		return Degraded
	default:
		return Green
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
		fs = append(fs, InfraField(FieldForgejo, r.Rig, r.Forgejo, in.Now),
			InfraField(FieldRunner, r.Rig, r.Runner, in.Now))
	}
	return fs
}

// InfraField is one probe's field. The tag is RECORDED: the probe is the
// patrol's, made on its own cadence, and this computation reads the record it
// left rather than reaching the instance itself. It is exported so the daemon's
// patrol raises its escalation on exactly the text the line shows.
func InfraField(name, rig string, p InfraProbe, now time.Time) Field {
	verdict := InfraVerdict(p, now)
	if name == FieldRunner {
		verdict = InfraRunnerVerdict(p, now)
	}
	switch verdict {
	case VerdictUnknown:
		reason := p.Unavailable
		if reason == "" {
			reason = "the probe has not run yet"
		}
		return unknown(name, rig, "", errors.New(reason))
	case Green:
		return Field{Name: name, Rig: rig, Tag: Recorded, Verdict: Green, Value: infraOKValue(name, p, now)}
	default:
		return Field{
			Name: name, Rig: rig, Tag: Recorded, Verdict: verdict,
			Value:  infraFailValue(name, p, now),
			Detail: infraFailDetail(name, p, now),
		}
	}
}

// infraOKValue says what the probe saw when it is healthy: the version says
// ok, and the runner queue names how long the oldest run has been waiting, or
// that nothing is.
func infraOKValue(name string, p InfraProbe, now time.Time) string {
	if name != FieldRunner {
		return "ok"
	}
	if p.WaitingSince.IsZero() {
		return "no waiting runs"
	}
	return "oldest waiting " + Short(now.Sub(p.WaitingSince))
}

// infraFailValue says what is wrong and for how long: the duration is what
// tells a reader whether the field is about to turn red.
func infraFailValue(name string, p InfraProbe, now time.Time) string {
	if name == FieldRunner {
		return "none picked up " + Short(now.Sub(p.WaitingSince))
	}
	return "down " + Short(infraAge(p, now))
}

// infraFailDetail names the failure in one phrase. The runner field names the
// run that is waiting, so an operator can open it (gt-fn9e6.56).
func infraFailDetail(name string, p InfraProbe, now time.Time) string {
	if name != FieldRunner {
		return p.Detail
	}
	age := Short(now.Sub(p.WaitingSince))
	if p.WaitingRun > 0 {
		return fmt.Sprintf("run %d has been waiting on a runner for %s", p.WaitingRun, age)
	}
	return "a run has been waiting on a runner for " + age
}

// infraAge is how long the failing run has lasted; zero on a probe that
// recorded no start, which the rule then reads as just begun.
func infraAge(p InfraProbe, now time.Time) time.Duration {
	if p.Since.IsZero() {
		return 0
	}
	return now.Sub(p.Since)
}
