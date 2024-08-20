package main

import "path/filepath"

type UnsafePath struct {
	rootBase     string
	relativePath string
}

func NewUnsafe(rootBase string, relativePath string) *UnsafePath {
	return &UnsafePath{
		rootBase:     rootBase,
		relativePath: relativePath,
	}
}

func UnsafeAbsolute(path *UnsafePath) string {
	return filepath.Join(path.rootBase, path.relativePath)
}

func UnsafeRelative(path *UnsafePath) string {
	return path.relativePath
}

func UnsafeRoot(path *UnsafePath) string {
	return path.rootBase
}
