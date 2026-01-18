package view

// ViewType represents which view layer to use.
type ViewType rune

const (
	ViewNone  ViewType = 0
	ViewHuman ViewType = 'H'
	ViewJSON  ViewType = 'J'
)

// String returns the string representation of the ViewType.
func (vt ViewType) String() string {
	switch vt {
	case ViewNone:
		return "none"
	case ViewHuman:
		return "human"
	case ViewJSON:
		return "json"
	default:
		return "unknown"
	}
}
