package metrics

import (
	"testing"
)

// bandsByLabel indexes bands by their label, so a test reads one band by name.
func bandsByLabel(t *testing.T, bands []EvalBand) map[string]EvalBand {
	t.Helper()
	if len(bands) != len(evalBandLabels) {
		t.Fatalf("evalBands returned %d bands, want %d", len(bands), len(evalBandLabels))
	}
	byLabel := make(map[string]EvalBand, len(bands))
	for i, b := range bands {
		if b.Label != evalBandLabels[i] {
			t.Fatalf("band %d is %q, want %q", i, b.Label, evalBandLabels[i])
		}
		byLabel[b.Label] = b
	}
	return byLabel
}

func TestEvalBands(t *testing.T) {
	t.Run("edges", func(t *testing.T) {
		// Each blended value lands in exactly one band, the one its label names.
		cases := []struct {
			blended float64
			want    string
		}{
			{5.99, "under 6"},
			{6, "6 to 8"},
			{8, "8 to 9"},
			{9, "9 to 10"},
			{10, "10 to 10.5"},
			{10.5, "10.5 to 12"},
			{12, "12 and over"},
		}
		for _, tc := range cases {
			rows := []EvalRow{evalTestRow("edge", 1, tc.blended, nil, nil, nil)}
			byLabel := bandsByLabel(t, evalBands(rows))
			for _, label := range evalBandLabels {
				wantCount := 0
				if label == tc.want {
					wantCount = 1
				}
				if got := byLabel[label].Count; got != wantCount {
					t.Errorf("blended %v: band %q has count %d, want %d", tc.blended, label, got, wantCount)
				}
			}
		}
	})

	t.Run("fixture", func(t *testing.T) {
		// Hand-computed figures. Every median and share is exact in float64.
		rows := []EvalRow{
			// under 6
			evalTestRow("u1", 4, 5, evalMinutes(3), evalMinutes(4), nil),
			evalTestRow("u2", 6, 5.99, evalMinutes(5), nil, evalMinutes(7)),
			evalTestRow("u3", 30, 5.5, evalMinutes(1), evalMinutes(2), evalMinutes(9)),
			evalTestRow("u4", 1, 2, nil, nil, nil),
			// 6 to 8: the actual 10.5 is not over 10.5
			evalTestRow("m1", 10.5, 6, evalMinutes(8), evalMinutes(6), evalMinutes(10)),
			evalTestRow("m2", 12, 7.5, nil, evalMinutes(8), nil),
			// 8 to 9: one row, with only a shape input
			evalTestRow("e1", 25, 8, evalMinutes(10), nil, nil),
			// 9 to 10 is empty
			// 10 to 10.5
			evalTestRow("t1", 14, 10, evalMinutes(11), evalMinutes(12), evalMinutes(13)),
			evalTestRow("t2", 8, 10.25, evalMinutes(9), nil, evalMinutes(15)),
			// 10.5 to 12
			evalTestRow("h1", 21, 10.5, evalMinutes(12), evalMinutes(10), evalMinutes(11)),
			evalTestRow("h2", 10.5, 11.5, evalMinutes(14), evalMinutes(9), evalMinutes(12)),
			// 12 and over
			evalTestRow("w1", 60, 40, evalMinutes(50), evalMinutes(45), evalMinutes(70)),
			evalTestRow("w2", 12, 12, nil, nil, nil),
		}
		byLabel := bandsByLabel(t, evalBands(rows))

		want := map[string]EvalBand{
			"under 6": {
				Count:            4,
				ActualMedian:     evalMinutes(5),
				ShapeMedian:      evalMinutes(3),
				EmbedMedian:      evalMinutes(3),
				JevMedian:        evalMinutes(8),
				BlendedMedian:    evalMinutes(5.25),
				OverTenHalfShare: 0.25,
				OverTwentyShare:  0.25,
			},
			"6 to 8": {
				Count:            2,
				ActualMedian:     evalMinutes(11.25),
				ShapeMedian:      evalMinutes(8),
				EmbedMedian:      evalMinutes(7),
				JevMedian:        evalMinutes(10),
				BlendedMedian:    evalMinutes(6.75),
				OverTenHalfShare: 0.5,
				OverTwentyShare:  0,
			},
			"8 to 9": {
				Count:            1,
				ActualMedian:     evalMinutes(25),
				ShapeMedian:      evalMinutes(10),
				EmbedMedian:      nil,
				JevMedian:        nil,
				BlendedMedian:    evalMinutes(8),
				OverTenHalfShare: 1,
				OverTwentyShare:  1,
			},
			"10 to 10.5": {
				Count:            2,
				ActualMedian:     evalMinutes(11),
				ShapeMedian:      evalMinutes(10),
				EmbedMedian:      evalMinutes(12),
				JevMedian:        evalMinutes(14),
				BlendedMedian:    evalMinutes(10.125),
				OverTenHalfShare: 0.5,
				OverTwentyShare:  0,
			},
			"10.5 to 12": {
				Count:            2,
				ActualMedian:     evalMinutes(15.75),
				ShapeMedian:      evalMinutes(13),
				EmbedMedian:      evalMinutes(9.5),
				JevMedian:        evalMinutes(11.5),
				BlendedMedian:    evalMinutes(11),
				OverTenHalfShare: 0.5,
				OverTwentyShare:  0.5,
			},
			"12 and over": {
				Count:            2,
				ActualMedian:     evalMinutes(36),
				ShapeMedian:      evalMinutes(50),
				EmbedMedian:      evalMinutes(45),
				JevMedian:        evalMinutes(70),
				BlendedMedian:    evalMinutes(26),
				OverTenHalfShare: 1,
				OverTwentyShare:  0.5,
			},
		}
		for label, w := range want {
			got := byLabel[label]
			if got.Count != w.Count {
				t.Errorf("%q: Count = %d, want %d", label, got.Count, w.Count)
			}
			checkMedian(t, label, "ActualMedian", got.ActualMedian, w.ActualMedian)
			checkMedian(t, label, "ShapeMedian", got.ShapeMedian, w.ShapeMedian)
			checkMedian(t, label, "EmbedMedian", got.EmbedMedian, w.EmbedMedian)
			checkMedian(t, label, "JevMedian", got.JevMedian, w.JevMedian)
			checkMedian(t, label, "BlendedMedian", got.BlendedMedian, w.BlendedMedian)
			if got.OverTenHalfShare != w.OverTenHalfShare {
				t.Errorf("%q: OverTenHalfShare = %v, want %v", label, got.OverTenHalfShare, w.OverTenHalfShare)
			}
			if got.OverTwentyShare != w.OverTwentyShare {
				t.Errorf("%q: OverTwentyShare = %v, want %v", label, got.OverTwentyShare, w.OverTwentyShare)
			}
		}

		empty := byLabel["9 to 10"]
		if empty.Count != 0 {
			t.Errorf("empty band: Count = %d, want 0", empty.Count)
		}
		checkMedian(t, "9 to 10", "ActualMedian", empty.ActualMedian, nil)
		checkMedian(t, "9 to 10", "BlendedMedian", empty.BlendedMedian, nil)
		if empty.OverTenHalfShare != 0 || empty.OverTwentyShare != 0 {
			t.Errorf("empty band: shares = %v, %v, want 0, 0", empty.OverTenHalfShare, empty.OverTwentyShare)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		byLabel := bandsByLabel(t, evalBands(nil))
		for _, label := range evalBandLabels {
			b := byLabel[label]
			if b.Count != 0 {
				t.Errorf("%q: Count = %d, want 0", label, b.Count)
			}
			checkMedian(t, label, "ActualMedian", b.ActualMedian, nil)
			checkMedian(t, label, "ShapeMedian", b.ShapeMedian, nil)
			checkMedian(t, label, "EmbedMedian", b.EmbedMedian, nil)
			checkMedian(t, label, "JevMedian", b.JevMedian, nil)
			checkMedian(t, label, "BlendedMedian", b.BlendedMedian, nil)
			if b.OverTenHalfShare != 0 || b.OverTwentyShare != 0 {
				t.Errorf("%q: shares = %v, %v, want 0, 0", label, b.OverTenHalfShare, b.OverTwentyShare)
			}
		}
	})
}

// checkMedian asserts a median is absent when want is nil and equal to want
// otherwise.
func checkMedian(t *testing.T, label, field string, got, want *float64) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%q: %s = %v, want none", label, field, *got)
	case want != nil && got == nil:
		t.Errorf("%q: %s = none, want %v", label, field, *want)
	case want != nil && *got != *want:
		t.Errorf("%q: %s = %v, want %v", label, field, *got, *want)
	}
}
