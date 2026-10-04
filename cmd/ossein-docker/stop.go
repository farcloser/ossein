//go:build darwin && arm64

package main

// stopCmd is `docker stop` with ossein's meaning: it stops background buildkit
// instances — the only long-lived thing ossein ever leaves running — by id.
// At least one id, as docker requires: `ossein stop` alone stops every
// instance on the machine, and a docker-shaped script never asked for that.
type stopCmd struct {
	IDs []string `arg:"" help:"ossein instance ids to stop (see 'ossein buildkit --detach' output)" name:"id"`
}

func (c *stopCmd) Run(level string) error {
	return execOssein(level, append([]string{"stop"}, c.IDs...)...)
}
