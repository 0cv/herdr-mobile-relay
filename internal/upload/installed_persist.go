package upload

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// Each lexical owner performs one close. Publication is irreversible here:
// directory open/sync/close failure must never authorize object rollback.
func (m *Manager) persistInstalledAttachments() (result error) {
	m.mu.Lock()
	records := make([]diskAttachmentRecord, 0, len(m.attachments))
	for _, record := range m.attachments {
		records = append(records, diskAttachmentRecord{Attachment: record.attachment, Target: newDiskAttachmentTarget(record.target), RelPath: record.relPath})
	}
	m.mu.Unlock()
	sort.Slice(records, func(i, j int) bool { return records[i].Attachment.Ref < records[j].Attachment.Ref })
	published := false
	failed := func(err error) error {
		m.witness(err)
		public := failure("upload_metadata_unavailable", nil)
		if published {
			return &attachmentIndexPublishedError{err: public}
		}
		return public
	}
	if err := m.deny("persist-create", attachmentIndexFilename); err != nil {
		return failed(err)
	}
	data, err := json.Marshal(attachmentIndex{SchemaVersion: attachmentIndexVersion, Attachments: records})
	if err != nil {
		return failed(err)
	}
	if err := m.deny("persist-temp-create", m.rootPath); err != nil {
		return failed(err)
	}
	temp, err := os.CreateTemp(m.rootPath, ".attachments-*.tmp")
	if err != nil {
		return failed(err)
	}
	name := temp.Name()
	owned := true
	defer func() {
		if owned {
			if err := m.physicalClose(temp); err != nil {
				result = failed(err)
			}
		}
		// After rename this lexical temporary no longer owns a pathname.
		if !published {
			err := m.deny("persist-temp-remove", name)
			if err == nil {
				err = os.Remove(name)
			}
			if err != nil {
				result = failed(err)
			}
		}
	}()
	if err := m.deny("persist-temp-chmod", name); err != nil {
		return failed(err)
	}
	if err := temp.Chmod(0o600); err != nil {
		return failed(err)
	}
	if err := m.deny("persist-temp-write", name); err != nil {
		return failed(err)
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		return failed(err)
	}
	if err := m.deny("persist-temp-sync", name); err != nil {
		return failed(err)
	}
	if err := temp.Sync(); err != nil {
		return failed(err)
	}
	owned = false
	if err := m.physicalClose(temp); err != nil {
		return failed(err)
	}
	if err := m.deny("persist-rename", name); err != nil {
		return failed(err)
	}
	if err := os.Rename(name, filepath.Join(m.rootPath, attachmentIndexFilename)); err != nil {
		return failed(err)
	}
	published = true
	if err := m.deny("persist-directory-open", m.rootPath); err != nil {
		return failed(err)
	}
	directory, err := os.Open(m.rootPath)
	if err != nil {
		return failed(err)
	}
	defer func() {
		if err := m.physicalClose(directory); err != nil {
			result = failed(err)
		}
	}()
	if err := m.deny("persist-directory-sync", m.rootPath); err != nil {
		return failed(err)
	}
	if err := directory.Sync(); err != nil {
		return failed(err)
	}
	return nil
}
