// Package godi is a stand-in for the real module, carrying only the two types
// the analyzers look at. The fixture must compile without the module cache.
package godi

// In marks a parameter object.
type In struct{}

// Out marks a result object.
type Out struct{}
