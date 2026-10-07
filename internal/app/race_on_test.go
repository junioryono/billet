//go:build race

package app

// raceBuild says this test binary was built with -race, so a `go list -export`
// it runs asks for the same variant and finds it in the build cache.
const raceBuild = true
