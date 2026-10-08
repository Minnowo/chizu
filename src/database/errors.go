package database

import "errors"

var (
	ErrNotFound          = errors.New("not found")
	ErrInvalidTokenizer  = errors.New("invalid tokenizer (want unicode61 or trigram)")
	ErrTokenizerMismatch = errors.New("tokenizer does not match the one this database was created with")
)
