package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/viper"

	"github.com/packwiz/packwiz/core"
	"github.com/spf13/cobra"
)

// refreshCmd represents the refresh command
var refreshCmd = &cobra.Command{
	Use:   "refresh",
	Short: "Refresh the index file",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Loading modpack...")
		pack, err := core.LoadPack()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		build, err := cmd.Flags().GetBool("build")
		if err == nil && build {
			viper.Set("no-internal-hashes", false)
		} else if viper.GetBool("no-internal-hashes") {
			fmt.Println("Note: no-internal-hashes mode is set, no hashes will be saved. Use --build to override this for distribution.")
		}
		index, err := pack.LoadIndex()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		noFollowSymlinks, err := cmd.Flags().GetBool("no-follow-symlinks")
		if err != nil {
			noFollowSymlinks = false
		}
		followSymlinks := !noFollowSymlinks
		err = index.RefreshWithOptions(followSymlinks)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		err = index.Write()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		err = pack.UpdateIndexHash()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		err = pack.Write()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println("Index refreshed!")
	},
}

func init() {
	rootCmd.AddCommand(refreshCmd)

	refreshCmd.Flags().Bool("build", false, "Only has an effect in no-internal-hashes mode: generates internal hashes for distribution with packwiz-installer")
	refreshCmd.Flags().Bool("no-follow-symlinks", false, "Don't follow symbolic links when refreshing the index (default is to follow them)")
}
