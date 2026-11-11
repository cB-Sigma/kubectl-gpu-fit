package cmd

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/gpu"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/pricing"
)

// newModelsCommand lists the built-in GPU catalog with the prices in effect.
func newModelsCommand(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "models",
		Short: "List known GPU models, their MIG profiles and the hourly prices in effect",
		Long: `List the GPU models kubectl gpu-fit knows about, the MIG profiles it may
recommend for each, and the hourly price used for cost estimates.

Built-in prices are rough on-demand list estimates. --price and --price-file
are honoured, so this is also a quick way to check your overrides.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			prices := pricing.Defaults()
			if o.priceFile != "" {
				if err := prices.LoadFile(o.priceFile); err != nil {
					return err
				}
			}
			if err := prices.ApplyOverrides(o.prices); err != nil {
				return err
			}
			w := tabwriter.NewWriter(c.OutOrStdout(), 0, 4, 3, ' ', 0)
			fmt.Fprintln(w, "MODEL\tGPU\tMEMORY\tUSD/HOUR\tPRICE SOURCE\tMIG PROFILES")
			for _, m := range gpu.Models() {
				price, source := "-", "-"
				if p, ok := prices.Lookup(m); ok {
					price, source = fmt.Sprintf("%.2f", p), "built-in estimate"
					if prices.Overridden(m) {
						source = "yours"
					}
				}
				var profiles []string
				for _, p := range m.MIG {
					profiles = append(profiles, p.Name)
				}
				mig := "-"
				if len(profiles) > 0 {
					mig = strings.Join(profiles, ", ")
				}
				fmt.Fprintf(w, "%s\t%s\t%d GB\t%s\t%s\t%s\n", m.Key, m.Display, m.MemoryGB, price, source, mig)
			}
			return w.Flush()
		},
	}
}
