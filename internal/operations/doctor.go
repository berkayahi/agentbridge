package operations

import (
	"context"
	"fmt"
)

type DoctorOptions struct{ Database string }

type DoctorReport struct {
	V2      bool             `json:"v2"`
	Healthy bool             `json:"healthy"`
	Counts  map[string]int64 `json:"counts,omitempty"`
}

func Doctor(ctx context.Context, options DoctorOptions) (DoctorReport, error) {
	if options.Database == "" {
		return DoctorReport{}, fmt.Errorf("doctor requires database")
	}
	db, err := openDatabase(ctx, options.Database)
	if err != nil {
		return DoctorReport{}, err
	}
	defer db.Close()
	if err := verifyV2(ctx, db); err != nil {
		return DoctorReport{V2: false, Healthy: false}, err
	}
	value, err := counts(ctx, db)
	if err != nil {
		return DoctorReport{V2: true, Healthy: false}, err
	}
	return DoctorReport{V2: true, Healthy: true, Counts: value}, nil
}
