package view

import (
	"encoding/json"
	"time"

	"github.com/fatih/color"
)

type ValidateView interface {
	Render(result ValidateResult)
}

type ValidateResult struct {
	FileCount int
	Errors    []ValidateFileError
}

type ValidateFileError struct {
	File    string
	Message string
}

func (r ValidateResult) HasErrors() bool {
	return len(r.Errors) > 0
}

// Human view implementation.

type validateHumanView struct {
	*HumanView
}

func newValidateHumanView(hv *HumanView) *validateHumanView {
	return &validateHumanView{HumanView: hv}
}

func (v *validateHumanView) Render(result ValidateResult) {
	if result.HasErrors() {
		for _, e := range result.Errors {
			v.Println(color.RGB(229, 50, 50).Sprintf("Error!"), e.File+":", e.Message)
		}
		return
	}

	v.Println(color.RGB(50, 108, 229).Sprintf("Valid!"), "no errors found.")
}

// JSON view implementation.

type validateJSONView struct {
	*JSONView
}

func newValidateJSONView(jv *JSONView) *validateJSONView {
	return &validateJSONView{JSONView: jv}
}

type validateJSONResult struct {
	Type      string              `json:"type"`
	Status    string              `json:"status"`
	Timestamp time.Time           `json:"timestamp"`
	Files     int                 `json:"files"`
	Errors    []ValidateFileError `json:"errors,omitempty"`
}

func (v *validateJSONView) Render(result ValidateResult) {
	out := validateJSONResult{
		Type:      "validate",
		Timestamp: time.Now(),
		Files:     result.FileCount,
	}

	if result.HasErrors() {
		out.Status = "error"
		out.Errors = result.Errors
	} else {
		out.Status = "success"
	}

	if data, err := json.Marshal(out); err == nil {
		v.Println(string(data))
	}
}

func NewValidateView(v Viewer) ValidateView {
	switch vt := v.(type) {
	case *HumanView:
		return newValidateHumanView(vt)
	case *JSONView:
		return newValidateJSONView(vt)
	default:
		panic("unknown view type")
	}
}
