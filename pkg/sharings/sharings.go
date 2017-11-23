package sharings

import (
	"strings"

	"github.com/cozy/cozy-stack/client/auth"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/contacts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/globals"
	"github.com/cozy/cozy-stack/pkg/instance"
	"github.com/cozy/cozy-stack/pkg/jobs"
	"github.com/cozy/cozy-stack/pkg/permissions"
	"github.com/cozy/cozy-stack/pkg/scheduler"
)

const (
	// WorkerTypeSharingUpdates is the string representation of the type of
	// workers that deals with updating sharings.
	WorkerTypeSharingUpdates = "sharingupdates"
)

// CreateSharingParams is filled from the request for creating a sharing.
type CreateSharingParams struct {
	SharingType string          `json:"sharing_type"`
	Permissions permissions.Set `json:"permissions"`
	Recipients  []string        `json:"recipients"`
	Description string          `json:"description,omitempty"`
	PreviewPath string          `json:"preview_path,omitempty"`
}

// Sharing contains all the information about a sharing.
// For clarification:
// * `SID` is generated by CouchDB and is the id of the sharing document.
// * `SharingID` is the actual id of the Sharing, generated by the stack.
type Sharing struct {
	SID         string          `json:"_id,omitempty"`
	SRev        string          `json:"_rev,omitempty"`
	SharingType string          `json:"sharing_type"`
	Permissions permissions.Set `json:"permissions,omitempty"`
	Sharer      Sharer          `json:"sharer,omitempty"`
	// TODO rename RecipientsStatus in Recipients
	RecipientsStatus []*RecipientStatus `json:"recipients,omitempty"`
	Description      string             `json:"description,omitempty"`
	PreviewPath      string             `json:"preview_path,omitempty"`
	AppSlug          string             `json:"app_slug"`
	Owner            bool               `json:"owner"`
	Revoked          bool               `json:"revoked,omitempty"`
}

// Sharer contains the information about the sharer from the recipient's
// perspective.
//
// ATTENTION: This structure will only be filled by the recipients as it is
// recipient specific. The `ClientID` is different for each recipient and only
// known by them.
type Sharer struct {
	URL          string           `json:"url"`
	SharerStatus *RecipientStatus `json:"sharer_status"`
}

// SharingRequestParams contains the basic information required to request
// a sharing party
type SharingRequestParams struct {
	SharingID       string `json:"state"`
	ClientID        string `json:"client_id"`
	InboundClientID string `json:"inbound_client_id"`
	Code            string `json:"code"`
}

// SharingMessage describes the message that will be transmitted to the workers
// "sharing_update" and "share_data".
type SharingMessage struct {
	SharingID string           `json:"sharing_id"`
	Rule      permissions.Rule `json:"rule"`
}

// RecipientInfo describes the recipient information that will be transmitted to
// the sharing workers.
type RecipientInfo struct {
	URL         string
	Scheme      string
	Client      auth.Client
	AccessToken auth.AccessToken
}

// WorkerData describes the basic data the workers need to process the events
// they will receive.
type WorkerData struct {
	DocID      string
	SharingID  string
	Selector   string
	Values     []string
	DocType    string
	Recipients []*RecipientInfo
}

// ID returns the sharing qualified identifier
func (s *Sharing) ID() string { return s.SID }

// Rev returns the sharing revision
func (s *Sharing) Rev() string { return s.SRev }

// DocType returns the sharing document type
func (s *Sharing) DocType() string { return consts.Sharings }

// Clone implements couchdb.Doc
func (s *Sharing) Clone() couchdb.Doc {
	cloned := *s
	if s.RecipientsStatus != nil {
		var rStatus []*RecipientStatus
		cloned.RecipientsStatus = rStatus
		for _, v := range s.RecipientsStatus {
			rec := *v
			cloned.RecipientsStatus = append(cloned.RecipientsStatus, &rec)
		}
	}
	if s.Sharer.SharerStatus != nil {
		sharerStatus := *s.Sharer.SharerStatus
		cloned.Sharer.SharerStatus = &sharerStatus
	}
	return &cloned
}

// SetID changes the sharing qualified identifier
func (s *Sharing) SetID(id string) { s.SID = id }

// SetRev changes the sharing revision
func (s *Sharing) SetRev(rev string) { s.SRev = rev }

// RecStatus returns the sharing recipients status
func (s *Sharing) RecStatus(db couchdb.Database) ([]*RecipientStatus, error) {
	var rStatus []*RecipientStatus

	for _, rec := range s.RecipientsStatus {
		recipient, err := GetRecipient(db, rec.RefRecipient.ID)
		if err != nil {
			return nil, err
		}
		rec.recipient = recipient
		rStatus = append(rStatus, rec)
	}

	s.RecipientsStatus = rStatus
	return rStatus, nil
}

// Recipients returns the sharing recipients
func (s *Sharing) Recipients(db couchdb.Database) ([]*contacts.Contact, error) {
	var recipients []*contacts.Contact

	for _, rec := range s.RecipientsStatus {
		recipient, err := GetRecipient(db, rec.RefRecipient.ID)
		if err != nil {
			return nil, err
		}
		rec.recipient = recipient
		recipients = append(recipients, recipient)
	}

	return recipients, nil
}

// GetSharingRecipientFromClientID returns the Recipient associated with the
// given clientID.
func (s *Sharing) GetSharingRecipientFromClientID(db couchdb.Database, clientID string) (*RecipientStatus, error) {
	for _, recStatus := range s.RecipientsStatus {
		if recStatus.Client.ClientID == clientID {
			return recStatus, nil
		}
	}
	return nil, ErrRecipientDoesNotExist
}

// GetRecipientStatusFromRecipientID returns the RecipientStatus associated with the
// given recipient ID.
func (s *Sharing) GetRecipientStatusFromRecipientID(db couchdb.Database, recID string) (*RecipientStatus, error) {
	for _, recStatus := range s.RecipientsStatus {
		if recStatus.recipient == nil {
			r, err := GetRecipient(db, recStatus.RefRecipient.ID)
			if err != nil {
				return nil, err
			}
			recStatus.recipient = r
		}
		if recStatus.recipient.ID() == recID {
			return recStatus, nil
		}
	}
	return nil, ErrRecipientDoesNotExist
}

// CheckSharingType returns an error if the sharing type is incorrect
func CheckSharingType(sharingType string) error {
	switch sharingType {
	case consts.OneShotSharing, consts.OneWaySharing, consts.TwoWaySharing:
		return nil
	}
	return ErrBadSharingType
}

// CreateSharing checks the sharing, creates the document in
// base and starts the sharing process by registering the sharer at each
// recipient as a new OAuth client.
func CreateSharing(instance *instance.Instance, params *CreateSharingParams, slug string) (*Sharing, error) {
	sharingType := params.SharingType
	if err := CheckSharingType(sharingType); err != nil {
		return nil, err
	}

	sharing := &Sharing{
		SharingType:      sharingType,
		Permissions:      params.Permissions,
		RecipientsStatus: make([]*RecipientStatus, 0, len(params.Recipients)),
		Description:      params.Description,
		PreviewPath:      params.PreviewPath,
		AppSlug:          slug,
		Owner:            true,
		Revoked:          false,
	}

	// Fetch the recipients in the database and populate RecipientsStatus
	for _, contactID := range params.Recipients {
		contact, err := GetRecipient(instance, contactID)
		if err != nil {
			continue
		}
		recipient := &RecipientStatus{
			Status: consts.SharingStatusPending,
			RefRecipient: couchdb.DocReference{
				Type: consts.Contacts,
				ID:   contact.DocID,
			},
			recipient: contact,
		}
		sharing.RecipientsStatus = append(sharing.RecipientsStatus, recipient)
	}
	if len(sharing.RecipientsStatus) == 0 {
		return nil, ErrRecipientDoesNotExist // TODO better error
	}

	if err := couchdb.CreateDoc(instance, sharing); err != nil {
		return nil, err
	}
	return sharing, nil
}

// FindSharing retrieves a sharing document from its ID
func FindSharing(db couchdb.Database, sharingID string) (*Sharing, error) {
	var res *Sharing
	err := couchdb.GetDoc(db, consts.Sharings, sharingID, res)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// FindSharingRecipient retrieve a sharing recipient from its clientID and sharingID
func FindSharingRecipient(db couchdb.Database, sharingID, clientID string) (*Sharing, *RecipientStatus, error) {
	sharing, err := FindSharing(db, sharingID)
	if err != nil {
		return nil, nil, err
	}
	sRec, err := sharing.GetSharingRecipientFromClientID(db, clientID)
	if err != nil {
		return nil, nil, err
	}
	if sRec == nil {
		return nil, nil, ErrRecipientDoesNotExist
	}
	return sharing, sRec, nil
}

// AddTrigger creates a new trigger on the updates of the shared documents
// The delTrigger flag is when the trigger must only listen deletions, i.e.
// an one-way on the recipient side, for the revocation
func AddTrigger(instance *instance.Instance, rule permissions.Rule, sharingID string, delTrigger bool) error {
	sched := globals.GetScheduler()

	var eventArgs string
	if rule.Selector != "" {
		eventArgs = rule.Type + ":CREATED,UPDATED,DELETED:" +
			strings.Join(rule.Values, ",") + ":" + rule.Selector
	} else {
		if delTrigger {
			eventArgs = rule.Type + ":DELETED:" +
				strings.Join(rule.Values, ",")
		} else {
			eventArgs = rule.Type + ":CREATED,UPDATED,DELETED:" +
				strings.Join(rule.Values, ",")
		}

	}

	msg := SharingMessage{
		SharingID: sharingID,
		Rule:      rule,
	}

	workerArgs, err := jobs.NewMessage(msg)
	if err != nil {
		return err
	}
	t, err := scheduler.NewTrigger(&scheduler.TriggerInfos{
		Type:       "@event",
		WorkerType: WorkerTypeSharingUpdates,
		Domain:     instance.Domain,
		Arguments:  eventArgs,
		Message:    workerArgs,
	})
	if err != nil {
		return err
	}
	instance.Logger().Infof("[sharings] AddTrigger: trigger created for "+
		"sharing %s", sharingID)

	return sched.Add(t)
}

// ExtractRecipientInfo returns a RecipientInfo from a RecipientStatus
func ExtractRecipientInfo(db couchdb.Database, rec *RecipientStatus) (*RecipientInfo, error) {
	recipient, err := GetRecipient(db, rec.RefRecipient.ID)
	if err != nil {
		return nil, err
	}
	u, scheme, err := ExtractDomainAndScheme(recipient)
	if err != nil {
		return nil, err
	}
	info := &RecipientInfo{
		URL:         u,
		Scheme:      scheme,
		AccessToken: rec.AccessToken,
		Client:      rec.Client,
	}
	return info, nil
}

var (
	_ couchdb.Doc = &Sharing{}
)
