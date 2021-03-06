package model

type UserSource int

const (
	UserSourceUnknown  UserSource = -1
	UserSourceGhostbin            = iota
	UserSourceMozillaPersona
)

type User interface {
	GetID() uint
	GetName() string

	GetSource() UserSource
	SetSource(UserSource)

	UpdateChallenge(password string)
	Check(password string) bool

	Permissions(class PermissionClass, args ...interface{}) PermissionScope

	GetPastes() ([]PasteID, error)
}
