package sample

const Limit = 10

var Default = New()

type Store struct{}

type Reader interface {
	Read() error
}

type ID = int

func (s *Store) Open() error {
	return nil
}

func (s Store) Close() {}

func New() *Store {
	return &Store{}
}
