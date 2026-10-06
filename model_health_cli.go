package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func modelHealthCmd(args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	s, err := provider.LoadModelHealth()
	if err != nil {
		return err
	}
	switch args[0] {
	case "on":
		if len(args) > 3 {
			return fmt.Errorf("usage: magpie provider health on [minutes [failures]]")
		}
		values := []*int{&s.IntervalMinutes, &s.FailureThreshold}
		for i, value := range args[1:] {
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("health setting %q must be an integer", value)
			}
			*values[i] = n
		}
		s.Enabled = true
		if err := provider.ConfigureModelHealth(s.ModelHealthConfig); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "Automatic model checks enabled. Unknown models are hidden until they pass. Checking now…")
		s, err = provider.ScanModelHealth(context.Background())
	case "off":
		if len(args) != 1 {
			return fmt.Errorf("usage: magpie provider health off")
		}
		s.Enabled = false
		err = provider.ConfigureModelHealth(s.ModelHealthConfig)
	case "scan":
		if len(args) != 1 {
			return fmt.Errorf("usage: magpie provider health scan")
		}
		s, err = provider.ScanModelHealth(context.Background())
	case "status":
		if len(args) != 1 {
			return fmt.Errorf("usage: magpie provider health status")
		}
	default:
		return fmt.Errorf("usage: magpie provider health on [minutes [failures]] | off | scan | status")
	}
	if err != nil {
		return err
	}
	fmt.Printf("Model checks: enabled=%t, every=%dm, failure threshold=%d\nState: %s\n", s.Enabled, s.IntervalMinutes, s.FailureThreshold, provider.ModelHealthPath())
	var rows []provider.ModelHealthRecord
	for _, r := range s.Records {
		rows = append(rows, r)
	}
	slices.SortFunc(rows, func(a, b provider.ModelHealthRecord) int {
		return compareHealthRow(a.Provider+"/"+a.Model+"/"+a.KeyID, b.Provider+"/"+b.Model+"/"+b.KeyID)
	})
	for _, r := range rows {
		state := "hidden"
		if r.Ready {
			state = "ready"
		}
		fmt.Printf("%-6s %s/%s key=%s failures=%d checked=%s %s\n", state, r.Provider, r.Model, r.KeyID, r.Failures, r.CheckedAt.Format(time.RFC3339), r.Result.Error)
	}
	return nil
}

func compareHealthRow(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
