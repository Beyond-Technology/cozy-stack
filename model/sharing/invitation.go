package sharing

import (
	"context"
	"fmt"
	"net/url"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/mail"
	"github.com/cozy/cozy-stack/pkg/shortcut"
	"golang.org/x/sync/errgroup"
)

// SendInvitations sends invitation mails to the recipients that were in the
// mail-not-sent status (owner only)
func (s *Sharing) SendInvitations(inst *instance.Instance, perms *permission.Permission) error {
	if !s.Owner {
		return ErrInvalidSharing
	}
	if len(s.Members) != len(s.Credentials)+1 {
		return ErrInvalidSharing
	}
	sharer, desc := s.getSharerAndDescription(inst)
	canSendShortcut := s.Rules[0].DocType != consts.BitwardenOrganizations

	g, _ := errgroup.WithContext(context.Background())
	for i := range s.Members {
		m := &s.Members[i]
		if i == 0 || m.Status != MemberStatusMailNotSent { // i == 0 is for the owner
			continue
		}
		state := s.Credentials[i-1].State
		g.Go(func() error {
			link := m.InvitationLink(inst, s, state, perms)
			if m.Instance != "" && canSendShortcut {
				if err := m.SendShortcut(inst, s, link); err == nil {
					m.Status = MemberStatusPendingInvitation
					return nil
				}
			}
			if m.Email == "" {
				return ErrInvitationNotSent
			}
			if err := m.SendMail(inst, s, sharer, desc, link); err != nil {
				inst.Logger().WithNamespace("sharing").
					Errorf("Can't send email for %#v: %s", m.Email, err)
				return ErrInvitationNotSent
			}
			m.Status = MemberStatusPendingInvitation
			return nil
		})
	}
	errg := g.Wait()
	if err := couchdb.UpdateDoc(inst, s); err != nil {
		return err
	}
	return errg
}

// SendInvitationsToMembers sends mails from a recipient (open_sharing) to
// their contacts to invite them
func (s *Sharing) SendInvitationsToMembers(inst *instance.Instance, members []Member, states map[string]string) error {
	sharer, desc := s.getSharerAndDescription(inst)

	for _, m := range members {
		key := m.Email
		if key == "" {
			key = m.Instance
		}
		sent := false
		link := m.InvitationLink(inst, s, states[key], nil)
		if m.Instance != "" {
			if err := m.SendShortcut(inst, s, link); err != nil {
				sent = true
			}
		}
		if !sent {
			if m.Email == "" {
				return ErrInvitationNotSent
			}
			if err := m.SendMail(inst, s, sharer, desc, link); err != nil {
				inst.Logger().WithNamespace("sharing").
					Errorf("Can't send email for %#v: %s", m.Email, err)
				return ErrInvitationNotSent
			}
		}
		for i, member := range s.Members {
			if i == 0 {
				continue // skip the owner
			}
			var found bool
			if m.Email == "" {
				found = m.Instance == member.Instance
			} else {
				found = m.Email == member.Email
			}
			if found && member.Status == MemberStatusMailNotSent {
				s.Members[i].Status = MemberStatusPendingInvitation
				break
			}
		}
	}
	return couchdb.UpdateDoc(inst, s)
}

func (s *Sharing) getSharerAndDescription(inst *instance.Instance) (string, string) {
	sharer, _ := inst.PublicName()
	if sharer == "" {
		sharer = inst.Translate("Sharing Empty name")
	}
	desc := s.Description
	if desc == "" {
		desc = inst.Translate("Sharing Empty description")
	}
	return sharer, desc
}

// InvitationLink generates an HTTP link where the recipient can start the
// process of accepting the sharing
func (m *Member) InvitationLink(inst *instance.Instance, s *Sharing, state string, perms *permission.Permission) string {
	if s.Owner && s.PreviewPath != "" && perms != nil {
		var code string
		if perms.Codes != nil {
			if c, ok := perms.Codes[m.Email]; ok {
				code = c
			}
		}
		if perms.ShortCodes != nil {
			if c, ok := perms.ShortCodes[m.Email]; ok {
				code = c
			}
		}
		if code != "" {
			u := inst.SubDomain(s.AppSlug)
			u.Path = s.PreviewPath
			u.RawQuery = url.Values{"sharecode": {code}}.Encode()
			return u.String()
		}
	}

	query := url.Values{"state": {state}}
	path := fmt.Sprintf("/sharings/%s/discovery", s.SID)
	return inst.PageURL(path, query)
}

// SendMail sends an invitation mail to a recipient
func (m *Member) SendMail(inst *instance.Instance, s *Sharing, sharer, description, link string) error {
	addr := &mail.Address{
		Email: m.Email,
		Name:  m.PrimaryName(),
	}
	sharerMail, _ := inst.SettingsEMail()
	var action string
	if s.ReadOnlyRules() {
		action = inst.Translate("Mail Sharing Request Action Read")
	} else {
		action = inst.Translate("Mail Sharing Request Action Write")
	}
	docType := getDocumentType(inst, s)
	mailValues := map[string]interface{}{
		"SharerPublicName": sharer,
		"SharerEmail":      sharerMail,
		"Action":           action,
		"Description":      description,
		"DocType":          docType,
		"SharingLink":      link,
	}
	msg, err := job.NewMessage(mail.Options{
		Mode:           "from",
		To:             []*mail.Address{addr},
		TemplateName:   "sharing_request",
		TemplateValues: mailValues,
		RecipientName:  addr.Name,
		Layout:         mail.CozyCloudLayout,
	})
	if err != nil {
		return err
	}
	_, err = job.System().PushJob(inst, &job.JobRequest{
		WorkerType: "sendmail",
		Message:    msg,
	})
	return err
}

func getDocumentType(inst *instance.Instance, s *Sharing) string {
	rule := s.FirstFilesRule()
	if rule == nil {
		if len(s.Rules) > 0 && s.Rules[0].DocType == consts.BitwardenOrganizations {
			return inst.Translate("Notification Sharing Type Organization")
		}
		return inst.Translate("Notification Sharing Type Document")
	}
	_, err := inst.VFS().FileByID(rule.Values[0])
	if err != nil {
		return inst.Translate("Notification Sharing Type Directory")
	}
	return inst.Translate("Notification Sharing Type File")
}

// CreateShortcut is used to create a shortcut for a Cozy to Cozy sharing that
// has not yet been accepted.
func (s *Sharing) CreateShortcut(inst *instance.Instance, previewURL string, seen bool) error {
	dir, err := EnsureSharedWithMeDir(inst)
	if err != nil {
		return err
	}

	body := shortcut.Generate(previewURL)
	cm := vfs.NewCozyMetadata(s.Members[0].Instance)
	fileDoc, err := vfs.NewFileDoc(
		s.Description+".url",
		dir.DocID,
		int64(len(body)),
		nil, // Let the VFS compute the md5sum
		consts.ShortcutMimeType,
		"shortcut",
		cm.UpdatedAt,
		false, // Not executable
		false, // Not trashed
		nil,   // No tags
	)
	if err != nil {
		return err
	}
	fileDoc.CozyMetadata = cm
	status := "new"
	if seen {
		status = "seen"
	}
	fileDoc.Metadata = vfs.Metadata{
		"sharing": map[string]interface{}{
			"status": status,
		},
		"target": map[string]interface{}{
			"cozyMetadata": map[string]interface{}{
				"instance": s.Members[0].Instance,
			},
			"_type": s.Rules[0].DocType,
			"mime":  s.Rules[0].Mime,
		},
	}
	fileDoc.AddReferencedBy(couchdb.DocReference{
		ID:   s.SID,
		Type: consts.Sharings,
	})

	file, err := inst.VFS().CreateFile(fileDoc, nil)
	if err != nil {
		basename := fileDoc.DocName
		for i := 2; i < 100; i++ {
			fileDoc.DocName = fmt.Sprintf("%s (%d)", basename, i)
			file, err = inst.VFS().CreateFile(fileDoc, nil)
			if err == nil {
				break
			}
		}
		if err != nil {
			return err
		}
	}
	_, err = file.Write(body)
	if cerr := file.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}

	s.ShortcutID = fileDoc.DocID
	if err := couchdb.UpdateDoc(inst, s); err != nil {
		inst.Logger().Warnf("Cannot save shortcut id %s: %s", s.ShortcutID, err)
	}

	return s.SendShortcutMail(inst, fileDoc, previewURL)
}

// SendShortcut sends the HTTP request to the cozy of the recipient for adding
// a shortcut on the recipient's instance.
func (m *Member) SendShortcut(inst *instance.Instance, s *Sharing, link string) error {
	u, err := url.Parse(m.Instance)
	if err != nil || u.Host == "" {
		return ErrInvalidURL
	}

	creds := s.FindCredentials(m)
	if creds == nil {
		return ErrInvalidSharing
	}

	v := url.Values{}
	v.Add("shortcut", "true")
	v.Add("url", link)
	u.RawQuery = v.Encode()
	return m.CreateSharingRequest(inst, s, creds, u)
}

// SendShortcutMail will send a notification mail after a shortcut for a
// sharing has been created.
func (s *Sharing) SendShortcutMail(inst *instance.Instance, fileDoc *vfs.FileDoc, previewURL string) error {
	sharerName := s.Members[0].PublicName
	if sharerName == "" {
		sharerName = inst.Translate("Sharing Empty name")
	}
	var action string
	if s.ReadOnlyRules() {
		action = inst.Translate("Mail Sharing Request Action Read")
	} else {
		action = inst.Translate("Mail Sharing Request Action Write")
	}
	targetType := getTargetType(inst, fileDoc.Metadata)
	mailValues := map[string]interface{}{
		"SharerPublicName": sharerName,
		"Action":           action,
		"TargetType":       targetType,
		"TargetName":       s.Description,
		"SharingLink":      previewURL,
	}
	msg, err := job.NewMessage(mail.Options{
		Mode:           "noreply",
		TemplateName:   "notifications_sharing",
		TemplateValues: mailValues,
		Layout:         mail.CozyCloudLayout,
	})
	if err != nil {
		return err
	}
	_, err = job.System().PushJob(inst, &job.JobRequest{
		WorkerType: "sendmail",
		Message:    msg,
	})
	return err
}

func getTargetType(inst *instance.Instance, metadata map[string]interface{}) string {
	target, _ := metadata["target"].(map[string]interface{})
	if target["_type"] != consts.Files {
		return inst.Translate("Notification Sharing Type Document")
	}
	if target["mime"] == nil || target["mime"] == "" {
		return inst.Translate("Notification Sharing Type Directory")
	}
	return inst.Translate("Notification Sharing Type File")
}
