package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An actions_retention block with no days or keep_newest gets the documented
// defaults; an explicit value is the value read back.
func TestActionsRetentionDefaultsAndExplicitValues(t *testing.T) {
	t.Parallel()
	unset := DiskMaintenanceActionsRetention{}
	days, err := unset.RetentionDays()
	require.NoError(t, err)
	assert.Equal(t, 30, days)
	keep, err := unset.RetentionKeepNewest()
	require.NoError(t, err)
	assert.Equal(t, 200, keep)

	n, k := 14, 50
	set := DiskMaintenanceActionsRetention{Enabled: true, Days: &n, KeepNewest: &k}
	days, err = set.RetentionDays()
	require.NoError(t, err)
	assert.Equal(t, 14, days)
	keep, err = set.RetentionKeepNewest()
	require.NoError(t, err)
	assert.Equal(t, 50, keep)
}

// A value outside its range is refused by name, never read as the default. The
// upper bound on days keeps the step's cutoff inside a time.Duration.
func TestActionsRetentionRefusesAnOutOfRangeValue(t *testing.T) {
	t.Parallel()
	zero := 0
	_, err := (DiskMaintenanceActionsRetention{Days: &zero}).RetentionDays()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "actions_retention.days = 0: want from 1 to 36500")
	tooLong := DiskMaintenanceMaxActionsRetentionDays + 1
	_, err = (DiskMaintenanceActionsRetention{Days: &tooLong}).RetentionDays()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "actions_retention.days = 36501: want from 1 to 36500")
	_, err = (DiskMaintenanceActionsRetention{KeepNewest: &zero}).RetentionKeepNewest()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "actions_retention.keep_newest = 0")
}
