package permissions

import (
	"encoding/json"
	"fmt"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/web/jsonapi"
	"github.com/labstack/echo"
)

// Permission is a storable object containing a set of rules and
// several codes
type Permission struct {
	PID         string            `json:"_id,omitempty"`
	PRev        string            `json:"_rev,omitempty"`
	Type        string            `json:"type,omitempty"`
	SourceID    string            `json:"source_id,omitempty"`
	Permissions *Set              `json:"permissions,omitempty"`
	ExpiresAt   int               `json:"expires_at,omitempty"`
	Codes       map[string]string `json:"codes,omitempty"`
}

const (
	// TypeRegister is the value of Permission.Type for the temporary permissions
	// allowed by registerToken
	TypeRegister = "register"

	// TypeApplication if the value of Permission.Type for an application
	TypeApplication = "app"

	// TypeSharing if the value of Permission.Type for a share permission doc
	TypeSharing = "share"

	// TypeOauth if the value of Permission.Type for a oauth permission doc
	TypeOauth = "oauth"
)

// Index is the necessary index for this package
// used in instance creation
var Index = mango.IndexOnFields("source_id", "type")

const shareByCView = "byToken"

// Views are athe necessary views for this package
// used in instance creation
var Views = couchdb.Views{
	shareByCView: couchdb.View{
		Map: `
function(doc){
	if(doc.type === "share" && doc.codes)
		Object.keys(doc.codes).forEach(function(k){ emit(doc.codes[k]) })
}`,
	},
}

var (
	// ErrNotSubset is returned on requests attempting to create a Set of
	// permissions which is not a subset of the request's own token.
	ErrNotSubset = echo.NewHTTPError(403, "attempt to create a larger permission set")

	// ErrOnlyAppCanCreateSubSet is returned if a non-app attempts to create
	// sharing permissions.
	ErrOnlyAppCanCreateSubSet = echo.NewHTTPError(403, "only apps can create sharing permissions")
)

// ID implements jsonapi.Doc
func (p *Permission) ID() string { return p.PID }

// Rev implements jsonapi.Doc
func (p *Permission) Rev() string { return p.PRev }

// DocType implements jsonapi.Doc
func (p *Permission) DocType() string { return consts.Permissions }

// SetID implements jsonapi.Doc
func (p *Permission) SetID(id string) { p.PID = id }

// SetRev implements jsonapi.Doc
func (p *Permission) SetRev(rev string) { p.PRev = rev }

// Relationships implements jsonapi.Doc
func (p *Permission) Relationships() jsonapi.RelationshipMap { return nil }

// Included implements jsonapi.Doc
func (p *Permission) Included() []jsonapi.Object { return nil }

// Links implements jsonapi.Doc
func (p *Permission) Links() *jsonapi.LinksList {
	return &jsonapi.LinksList{Self: "/permissions/" + p.PID}
}

// GetForRegisterToken create a non-persisted permissions doc with hard coded
// registerToken permissions set
func GetForRegisterToken() *Permission {
	return &Permission{
		Type: TypeRegister,
		Permissions: &Set{
			Rule{
				Verbs:  Verbs(GET),
				Type:   consts.Settings,
				Values: []string{consts.InstanceSettingsID},
			},
		},
	}
}

// GetForOauth create a non-persisted permissions doc from a oauth token scopes
func GetForOauth(claims *Claims) (*Permission, error) {
	set, err := UnmarshalScopeString(claims.Scope)
	if err != nil {
		return nil, err
	}
	pdoc := &Permission{
		Type:        TypeOauth,
		Permissions: set,
	}
	return pdoc, nil
}

// GetForApp retrieves the Permission doc for a given app
func GetForApp(db couchdb.Database, slug string) (*Permission, error) {
	var res []Permission
	err := couchdb.FindDocs(db, consts.Permissions, &couchdb.FindRequest{
		Selector: mango.And(
			mango.Equal("type", TypeApplication),
			mango.Equal("source_id", consts.Apps+"/"+slug),
		),
	}, &res)
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("no permission doc for %v", slug)
	}
	return &res[0], nil
}

// GetForShareCode retrieves the Permission doc for a given sharing code
func GetForShareCode(db couchdb.Database, tokenCode string) (*Permission, error) {
	var res couchdb.ViewResponse
	err := couchdb.ExecView(db, &couchdb.ViewRequest{
		Doctype:     consts.Permissions,
		ViewName:    shareByCView,
		Key:         tokenCode,
		IncludeDocs: true,
	}, &res)
	if err != nil {
		return nil, err
	}

	if len(res.Rows) == 0 {
		return nil, fmt.Errorf("no permission doc for token %v", tokenCode)
	}

	if len(res.Rows) > 1 {
		return nil, fmt.Errorf("Bad state : several permission docs for token %v", tokenCode)
	}

	var pdoc Permission
	err = json.Unmarshal(*res.Rows[0].Doc, &pdoc)
	if err != nil {
		return nil, err
	}

	return &pdoc, nil
}

// CreateAppSet creates a Permission doc for an app
func CreateAppSet(db couchdb.Database, slug string, set Set) (*Permission, error) {
	existing, _ := GetForApp(db, slug)
	if existing != nil {
		return nil, fmt.Errorf("There is already a permission doc for %v", slug)
	}

	doc := &Permission{
		Type:        "app",
		SourceID:    consts.Apps + "/" + slug,
		Permissions: &set, // @TODO some validation?
	}

	err := couchdb.CreateDoc(db, doc)
	if err != nil {
		return nil, err
	}

	return doc, nil
}

// CreateShareSet creates a Permission doc for sharing
func CreateShareSet(db couchdb.Database, parent *Permission, codes map[string]string, set *Set) (*Permission, error) {

	if parent.Type == TypeRegister || parent.Type == TypeSharing {
		return nil, ErrOnlyAppCanCreateSubSet
	}

	if !set.IsSubSetOf(parent.Permissions) {
		return nil, ErrNotSubset
	}

	// SourceID stays the same, allow quick destruction of all children permissions
	doc := &Permission{
		Type:        TypeSharing,
		SourceID:    parent.SourceID,
		Permissions: set, // @TODO some validation?
		Codes:       codes,
	}

	err := couchdb.CreateDoc(db, doc)
	if err != nil {
		return nil, err
	}

	return doc, nil
}

// Force creates or updates a Permission doc for a given app
func Force(db couchdb.Database, slug string, set Set) error {
	existing, _ := GetForApp(db, slug)
	doc := &Permission{
		Type:        TypeApplication,
		SourceID:    consts.Apps + "/" + slug,
		Permissions: &set, // @TODO some validation?
	}
	if existing == nil {
		return couchdb.CreateDoc(db, doc)
	}

	doc.SetID(existing.ID())
	doc.SetRev(existing.Rev())
	return couchdb.UpdateDoc(db, doc)
}

// DestroyApp remove all Permission docs for a given app
func DestroyApp(db couchdb.Database, slug string) error {
	var res []Permission
	err := couchdb.FindDocs(db, consts.Permissions, &couchdb.FindRequest{
		Selector: mango.Equal("source_id", consts.Apps+"/"+slug),
	}, &res)
	if err != nil {
		return err
	}
	for _, p := range res {
		err := couchdb.DeleteDoc(db, &p)
		if err != nil {
			return err
		}
	}
	return nil
}
