package hooks

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	hookcfg "github.com/mathomhaus/guild/internal/hooks"
)

// scanReport is one harness's scan result in `guild hooks scan` output.
type scanReport struct {
	Harness string         `json:"harness"`
	Path    string         `json:"path"`
	Hooks   []hookcfg.Hook `json:"hooks"`
}

func newScanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "report the hooks currently present in each harness's settings",
		Long: `guild hooks scan: read-only inventory

Reads each registered adapter's settings file and reports every hook
found there, guild-owned and foreign alike. Foreign hooks are tagged so
you can see what else manages this harness. Nothing is written.

Flags:
  --verbose  also report harnesses with no hooks installed
  --json     machine-readable output`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			verbose, _ := cmd.Flags().GetBool("verbose")
			asJSON, _ := cmd.Flags().GetBool("json")
			if err := runScan(liveDeps(cmd.OutOrStdout()), verbose, asJSON); err != nil {
				return fmt.Errorf("guild hooks scan: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "machine-readable output")
	return cmd
}

func runScan(d deps, verbose, asJSON bool) error {
	if len(d.adapters) == 0 && !asJSON {
		fmt.Fprintln(d.out, "No hook adapters are registered in this build.")
		return nil
	}
	reports := make([]scanReport, 0, len(d.adapters))
	for _, ad := range d.adapters {
		path, err := ad.SettingsPath()
		if err != nil {
			return fmt.Errorf("settings path for %s: %w", ad.Name(), err)
		}
		hs, err := ad.Scan()
		if err != nil {
			return fmt.Errorf("scan %s: %w", ad.Name(), err)
		}
		if len(hs) == 0 && !verbose && !asJSON {
			continue
		}
		reports = append(reports, scanReport{Harness: ad.Name(), Path: path, Hooks: hs})
	}

	if asJSON {
		enc := json.NewEncoder(d.out)
		enc.SetIndent("", "  ")
		return enc.Encode(reports)
	}

	if len(reports) == 0 {
		fmt.Fprintln(d.out, "No hooks found in any harness settings file.")
		return nil
	}
	for _, r := range reports {
		fmt.Fprintf(d.out, "%s (%s):\n", r.Harness, r.Path)
		if len(r.Hooks) == 0 {
			fmt.Fprintln(d.out, "  (no hooks)")
			continue
		}
		for _, h := range r.Hooks {
			tag := "[foreign]"
			if h.GuildOwned {
				tag = "[guild]  "
			}
			fmt.Fprintf(d.out, "  %s %s\n", tag, formatHook(h))
		}
	}
	return nil
}
