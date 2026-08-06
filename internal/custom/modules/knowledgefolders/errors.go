package knowledgefolders

import "errors"

var (
	ErrFolderNotFound    = errors.New("knowledge folder not found")
	ErrFolderNameInvalid = errors.New("knowledge folder name is invalid")
	ErrFolderNameExists  = errors.New("a folder with the same name already exists under this parent")
	ErrFolderDepth       = errors.New("knowledge folder depth limit exceeded")
	ErrFolderCycle       = errors.New("a folder cannot be moved into itself or its descendant")
	ErrDocumentNotFound  = errors.New("one or more knowledge documents were not found in this knowledge base")
	ErrInvalidPage       = errors.New("invalid pagination parameters")
)
