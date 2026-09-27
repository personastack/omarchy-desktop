package testfixture

import (
	_ "embed"
	"encoding/json"
)

//go:embed desktop-parity.json
var desktopParity []byte

type DesktopParity struct {
	Files   FileCases    `json:"files"`
	Process ProcessCases `json:"process"`
}

type FileCases struct {
	UTF8Boundary   UTF8BoundaryCase   `json:"utf8Boundary"`
	Binary         BinaryCase         `json:"binary"`
	Image          ImageCase          `json:"image"`
	Search         SearchCase         `json:"search"`
	Changed        ChangedCase        `json:"changed"`
	Symlink        SymlinkCase        `json:"symlink"`
	Permission     PermissionCase     `json:"permission"`
	UncertainWrite UncertainWriteCase `json:"uncertainWrite"`
}

type UTF8BoundaryCase struct {
	ASCIIPrefixBytes int    `json:"asciiPrefixBytes"`
	TrailingBytesHex string `json:"trailingBytesHex"`
	FirstPageBytes   int    `json:"firstPageBytes"`
	NextOffset       int    `json:"nextOffset"`
	TailText         string `json:"tailText"`
}

type BinaryCase struct {
	BytesHex string `json:"bytesHex"`
	Encoding string `json:"encoding"`
	Base64   string `json:"base64"`
}

type ImageCase struct {
	Name     string `json:"name"`
	BytesHex string `json:"bytesHex"`
	MIMEType string `json:"mimeType"`
}

type SearchCase struct {
	NameGlob        string       `json:"nameGlob"`
	ContentContains string       `json:"contentContains"`
	Limit           int          `json:"limit"`
	Files           []SearchFile `json:"files"`
	Pages           [][]string   `json:"pages"`
}

type SearchFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type ChangedCase struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

type SymlinkCase struct {
	TargetName     string `json:"targetName"`
	TargetContents string `json:"targetContents"`
	LinkName       string `json:"linkName"`
	Kind           string `json:"kind"`
}

type PermissionCase struct {
	Mode int    `json:"mode"`
	Code string `json:"code"`
}

type UncertainWriteCase struct {
	Code             string `json:"code"`
	Message          string `json:"message"`
	KnownFailureCode string `json:"knownFailureCode"`
}

type ProcessCases struct {
	OutputGapBytes          int    `json:"outputGapBytes"`
	StdinText               string `json:"stdinText"`
	BlockedStdinWriteBytes  int    `json:"blockedStdinWriteBytes"`
	BlockedStdinWrites      int    `json:"blockedStdinWrites"`
	MaxProcesses            int    `json:"maxProcesses"`
	LimitCode               string `json:"limitCode"`
	BlockedInputCommand     string `json:"blockedInputCommand"`
	CancellationCommand     string `json:"cancellationCommand"`
	LeaderChildDelaySeconds int    `json:"leaderChildDelaySeconds"`
	LeaderChildMarker       string `json:"leaderChildMarker"`
}

func LoadDesktopParity() (DesktopParity, error) {
	var fixture DesktopParity
	err := json.Unmarshal(desktopParity, &fixture)
	return fixture, err
}
