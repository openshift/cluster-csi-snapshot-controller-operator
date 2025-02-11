package main

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/operator"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/version"
	"github.com/openshift/library-go/pkg/controller/controllercmd"

	"k8s.io/component-base/cli"
	"k8s.io/utils/clock"
)

func main() {
	command := NewCSISnapshotControllerOperatorCommand()
	code := cli.Run(command)
	os.Exit(code)
}

var (
	guestKubeconfig *string
)

func NewCSISnapshotControllerOperatorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "csi-snapshot-controller-operator",
		Short: "OpenShift CSI Snapshot operator",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help()
			os.Exit(1)
		},
	}

	cmd2 := controllercmd.NewControllerCommandConfig("csi-snapshot-controller-operator", version.Get(), runOperatorWithGuestKubeconfig, clock.RealClock{}).NewCommand()
	cmd2.Use = "start"
	cmd2.Short = "Start the CSI Snapshot Controller Operator"
	guestKubeconfig = cmd2.Flags().String("guest-kubeconfig", "", "Path to the guest kubeconfig file. This flag enables hypershift integration.")

	cmd.AddCommand(cmd2)

	return cmd
}

func runOperatorWithGuestKubeconfig(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	return operator.RunOperator(ctx, controllerConfig, *guestKubeconfig)
}
