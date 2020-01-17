package sharing

import (
	"encoding/hex"
	"fmt"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
)

type bulkRevs struct {
	Rev       string
	Revisions RevsStruct
}

type sharingIndexer struct {
	db       prefixer.Prefixer
	indexer  vfs.Indexer
	bulkRevs *bulkRevs
	shared   *SharedRef
}

// newSharingIndexer creates an Indexer for the special purpose of the sharing.
// It intercepts some requests to force the id and revisions of some documents,
// and proxifies other requests to the normal couchdbIndexer (reads).
func newSharingIndexer(inst *instance.Instance, bulkRevs *bulkRevs, shared *SharedRef) *sharingIndexer {
	return &sharingIndexer{
		db:       inst,
		indexer:  vfs.NewCouchdbIndexer(inst),
		bulkRevs: bulkRevs,
		shared:   shared,
	}
}

// IncrementRevision is used when a conflict between 2 files/folders arise: to
// resolve the conflict, a new name is changed and to ensure that this change
// is propagated to the other cozy instances, we add a new revision to the
// chain.
func (s *sharingIndexer) IncrementRevision() {
	if s.bulkRevs == nil {
		return
	}

	start := s.bulkRevs.Revisions.Start
	start++
	generated := hex.EncodeToString(crypto.GenerateRandomBytes(16))
	s.bulkRevs.Rev = fmt.Sprintf("%d-%s", start, generated)
	s.bulkRevs.Revisions.Start = start
	s.bulkRevs.Revisions.IDs = append([]string{generated}, s.bulkRevs.Revisions.IDs...)
}

// WillResolveConflict is used when a conflict on a file/folder has been detected.
// There are 2 cases:
// 1. the file/folder has a revision on one side with a generation strictly
// higher than the revision on the other side => we can use this revision on
// both sides (but the chain of parents will not be the same)
// 2. the two revisions for the file/folder are at the same generation => we
// have to create a new revision that will be propagated to the other cozy.
func (s *sharingIndexer) WillResolveConflict(rev string, chain []string) {
	last := chain[len(chain)-1]
	if RevGeneration(last) == RevGeneration(rev) {
		s.bulkRevs = nil
		return
	}

	altered := MixupChainToResolveConflict(rev, chain)
	s.bulkRevs.Revisions = revsChainToStruct(altered)
	s.bulkRevs.Rev = last
}

// StashRevision is a way to not use the last revision for the next operation,
// and to keep it for later.
//
// For a new file, at least 2 revisions are needed: one to stash, and the other
// for next operation. For an existing file, at least 3 revisions are needed
// for this to work (the first revision is the one that is already is CouchDB,
// the second is the revision for the next operation, and the third is the
// stash). If don't have them, we fallback to revisions generated by CouchDB.
func (s *sharingIndexer) StashRevision(newFile bool) string {
	minRevs := 3
	if newFile {
		minRevs = 2
	}
	if s.bulkRevs == nil || len(s.bulkRevs.Revisions.IDs) < minRevs {
		s.bulkRevs = nil
		return ""
	}
	stash := s.bulkRevs.Revisions.IDs[0]
	s.bulkRevs.Revisions.IDs = s.bulkRevs.Revisions.IDs[1:]
	s.bulkRevs.Revisions.Start--
	s.bulkRevs.Rev = fmt.Sprintf("%d-%s", s.bulkRevs.Revisions.Start, s.bulkRevs.Revisions.IDs[0])
	return stash
}

// UnstashRevision takes back a stash returned by StashRevision after the
// intermediate operation has been done.
func (s *sharingIndexer) UnstashRevision(stash string) {
	if s.bulkRevs == nil {
		return
	}
	s.bulkRevs.Revisions.Start++
	s.bulkRevs.Revisions.IDs = append([]string{stash}, s.bulkRevs.Revisions.IDs...)
	s.bulkRevs.Rev = fmt.Sprintf("%d-%s", s.bulkRevs.Revisions.Start, stash)
}

// CreateBogusPrevRev creates a fake revision that can be used for an operation
// that come before the revision of bulkRevs.
func (s *sharingIndexer) CreateBogusPrevRev() {
	if s.bulkRevs == nil {
		return
	}
	bogus := hex.EncodeToString(crypto.GenerateRandomBytes(16))
	s.bulkRevs.Revisions.IDs = append(s.bulkRevs.Revisions.IDs, bogus)
}

func (s *sharingIndexer) InitIndex() error {
	return ErrInternalServerError
}

func (s *sharingIndexer) DiskUsage() (int64, error) {
	return s.indexer.DiskUsage()
}

func (s *sharingIndexer) FilesUsage() (int64, error) {
	return s.indexer.FilesUsage()
}

func (s *sharingIndexer) VersionsUsage() (int64, error) {
	return s.indexer.VersionsUsage()
}

func (s *sharingIndexer) TrashUsage() (int64, error) {
	return s.indexer.TrashUsage()
}

func (s *sharingIndexer) CreateFileDoc(doc *vfs.FileDoc) error {
	return ErrInternalServerError
}

func (s *sharingIndexer) CreateNamedFileDoc(doc *vfs.FileDoc) error {
	if s.bulkRevs == nil {
		return s.indexer.CreateNamedFileDoc(doc)
	}

	// If the VFS creates the file by omitting the fake first revision with
	// trashed=true, it is easy: we can insert the doc as is, and trigger the
	// realtime event.
	if !doc.Trashed {
		// Ensure that fullpath is filled because it's used in realtime/@events
		if _, err := doc.Path(s); err != nil {
			logger.WithNamespace("sharing-indexer").
				Errorf("Cannot compute fullpath for %#v: %s", doc, err)
			return err
		}
		if err := s.bulkForceUpdateDoc(doc); err != nil {
			return err
		}
		couchdb.RTEvent(s.db, realtime.EventCreate, doc, nil)
		return nil
	}

	// But if the VFS creates a first fake revision, it will also create
	// another revision after that to clear the trashed attribute when the
	// upload will complete. It means using 2 revision numbers. So, we have to
	// stash the target revision during the first write to keep it for the
	// second write.
	if len(s.bulkRevs.Revisions.IDs) == 1 {
		s.CreateBogusPrevRev()
	}
	stash := s.StashRevision(true)
	err := s.bulkForceUpdateDoc(doc)
	s.UnstashRevision(stash)
	return err
}

func (s *sharingIndexer) UpdateFileDoc(olddoc, doc *vfs.FileDoc) error {
	if s.bulkRevs == nil {
		return s.indexer.UpdateFileDoc(olddoc, doc)
	}

	if err := s.bulkForceUpdateDoc(doc); err != nil {
		return err
	}

	if s.shared != nil {
		if err := UpdateFileShared(s.db, s.shared, s.bulkRevs.Revisions); err != nil {
			return err
		}
	}

	// Ensure that fullpath is filled because it's used in realtime/@events
	if _, err := doc.Path(s); err != nil {
		return err
	}
	if olddoc != nil {
		if _, err := olddoc.Path(s); err != nil {
			return err
		}
		couchdb.RTEvent(s.db, realtime.EventUpdate, doc, olddoc)
	} else {
		couchdb.RTEvent(s.db, realtime.EventUpdate, doc, nil)
	}
	return nil
}

func (s *sharingIndexer) bulkForceUpdateDoc(doc *vfs.FileDoc) error {
	docs := make([]map[string]interface{}, 1)
	docs[0] = map[string]interface{}{
		"type":       doc.Type,
		"_id":        doc.DocID,
		"name":       doc.DocName,
		"dir_id":     doc.DirID,
		"created_at": doc.CreatedAt,
		"updated_at": doc.UpdatedAt,
		"tags":       doc.Tags,
		"size":       fmt.Sprintf("%d", doc.ByteSize), // XXX size must be serialized as a string, not an int
		"md5sum":     doc.MD5Sum,
		"mime":       doc.Mime,
		"class":      doc.Class,
		"executable": doc.Executable,
		"trashed":    doc.Trashed,
	}
	if len(doc.ReferencedBy) > 0 {
		docs[0][couchdb.SelectorReferencedBy] = doc.ReferencedBy
	}
	if doc.Metadata != nil {
		docs[0]["metadata"] = doc.Metadata
	}
	if doc.CozyMetadata != nil {
		docs[0]["cozyMetadata"] = doc.CozyMetadata
	}
	if doc.InternalID != "" {
		docs[0]["internal_vfs_id"] = doc.InternalID
	}
	doc.SetRev(s.bulkRevs.Rev)
	docs[0]["_rev"] = s.bulkRevs.Rev
	docs[0]["_revisions"] = s.bulkRevs.Revisions
	return couchdb.BulkForceUpdateDocs(s.db, consts.Files, docs)
}

// DeleteFileDoc is used when uploading a new file fails (invalid md5sum for example)
func (s *sharingIndexer) DeleteFileDoc(doc *vfs.FileDoc) error {
	return s.indexer.DeleteFileDoc(doc)
}

func (s *sharingIndexer) CreateDirDoc(doc *vfs.DirDoc) error {
	return ErrInternalServerError
}

func (s *sharingIndexer) CreateNamedDirDoc(doc *vfs.DirDoc) error {
	return s.UpdateDirDoc(nil, doc)
}

func (s *sharingIndexer) UpdateDirDoc(olddoc, doc *vfs.DirDoc) error {
	if s.bulkRevs == nil {
		return s.indexer.UpdateDirDoc(olddoc, doc)
	}

	docs := make([]map[string]interface{}, 1)
	docs[0] = map[string]interface{}{
		"type":       doc.Type,
		"_id":        doc.DocID,
		"name":       doc.DocName,
		"dir_id":     doc.DirID,
		"created_at": doc.CreatedAt,
		"updated_at": doc.UpdatedAt,
		"tags":       doc.Tags,
		"path":       doc.Fullpath,
	}
	if len(doc.ReferencedBy) > 0 {
		docs[0][couchdb.SelectorReferencedBy] = doc.ReferencedBy
	}
	if doc.CozyMetadata != nil {
		docs[0]["cozyMetadata"] = doc.CozyMetadata
	}
	doc.SetRev(s.bulkRevs.Rev)
	docs[0]["_rev"] = s.bulkRevs.Rev
	docs[0]["_revisions"] = s.bulkRevs.Revisions
	if err := couchdb.BulkForceUpdateDocs(s.db, consts.Files, docs); err != nil {
		return err
	}

	if err := UpdateFileShared(s.db, s.shared, s.bulkRevs.Revisions); err != nil {
		return err
	}

	if olddoc != nil {
		couchdb.RTEvent(s.db, realtime.EventUpdate, doc, olddoc)
	} else {
		couchdb.RTEvent(s.db, realtime.EventUpdate, doc, nil)
	}
	return nil
}

func (s *sharingIndexer) DeleteDirDoc(doc *vfs.DirDoc) error {
	return ErrInternalServerError
}

func (s *sharingIndexer) DeleteDirDocAndContent(doc *vfs.DirDoc, onlyContent bool) (files []*vfs.FileDoc, n int64, err error) {
	return nil, 0, ErrInternalServerError
}

func (s *sharingIndexer) BatchDelete(docs []couchdb.Doc) error {
	return ErrInternalServerError
}

func (s *sharingIndexer) DirByID(fileID string) (*vfs.DirDoc, error) {
	return s.indexer.DirByID(fileID)
}

func (s *sharingIndexer) DirByPath(name string) (*vfs.DirDoc, error) {
	return s.indexer.DirByPath(name)
}

func (s *sharingIndexer) FileByID(fileID string) (*vfs.FileDoc, error) {
	return s.indexer.FileByID(fileID)
}

func (s *sharingIndexer) FileByPath(name string) (*vfs.FileDoc, error) {
	return s.indexer.FileByPath(name)
}

func (s *sharingIndexer) FilePath(doc *vfs.FileDoc) (string, error) {
	return s.indexer.FilePath(doc)
}

func (s *sharingIndexer) DirOrFileByID(fileID string) (*vfs.DirDoc, *vfs.FileDoc, error) {
	return s.indexer.DirOrFileByID(fileID)
}

func (s *sharingIndexer) DirOrFileByPath(name string) (*vfs.DirDoc, *vfs.FileDoc, error) {
	return s.indexer.DirOrFileByPath(name)
}

func (s *sharingIndexer) DirIterator(doc *vfs.DirDoc, opts *vfs.IteratorOptions) vfs.DirIterator {
	return s.indexer.DirIterator(doc, opts)
}

func (s *sharingIndexer) DirBatch(doc *vfs.DirDoc, cursor couchdb.Cursor) ([]vfs.DirOrFileDoc, error) {
	return s.indexer.DirBatch(doc, cursor)
}

func (s *sharingIndexer) DirLength(doc *vfs.DirDoc) (int, error) {
	return s.indexer.DirLength(doc)
}

func (s *sharingIndexer) DirChildExists(dirID, name string) (bool, error) {
	return s.indexer.DirChildExists(dirID, name)
}

func (s *sharingIndexer) CreateVersion(v *vfs.Version) error {
	return s.indexer.CreateVersion(v)
}

func (s *sharingIndexer) DeleteVersion(v *vfs.Version) error {
	return s.indexer.DeleteVersion(v)
}

func (s *sharingIndexer) BatchDeleteVersions(versions []*vfs.Version) error {
	return s.indexer.BatchDeleteVersions(versions)
}

func (s *sharingIndexer) CheckIndexIntegrity(predicate func(*vfs.FsckLog), failFast bool) error {
	return ErrInternalServerError
}

func (s *sharingIndexer) CheckTreeIntegrity(tree *vfs.Tree, predicate func(*vfs.FsckLog), failFast bool) error {
	return ErrInternalServerError
}

func (s *sharingIndexer) BuildTree(each ...func(*vfs.TreeFile)) (t *vfs.Tree, err error) {
	return nil, ErrInternalServerError
}

var _ vfs.Indexer = (*sharingIndexer)(nil)
