//go:build darwin && arm64

package main

// pullCmd is `docker pull`, passed straight to `ossein pull`.
type pullCmd struct {
	Platform string `help:"linux/amd64 | linux/arm64 (default: host)"`
	Image    string `arg:""                                           help:"image reference"`
}

func (c *pullCmd) Run(level string) error {
	args := []string{"pull"}
	if c.Platform != "" {
		args = append(args, "--platform", c.Platform)
	}

	return execOssein(level, append(args, "--", c.Image)...)
}
