// Package setup is the commands that make a deployment: `billet init`, which
// generates a configuration (single host, hybrid, EC2, CodeBuild, Tart, the IAM
// policies), and `billet github-app`, which registers the GitHub App, writes its
// identity into the configuration in place and publishes its key.
package setup
