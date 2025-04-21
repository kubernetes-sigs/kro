// This file just exists as a place to put //go:generate directives that should apply to the entire project

package kro

//go:generate go tool attribution-gen
//go:generate go tool nwa config -c update
