package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
)

var ErrNotFound = errors.New("sprocket not found")

type Sprocket struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
}

type SprocketRepository interface {
	Create(context.Context, Sprocket) (Sprocket, error)
	Get(context.Context, string) (Sprocket, error)
	List(context.Context) ([]Sprocket, error)
	Delete(context.Context, string) error
}

type MemorySprocketRepository struct {
	mu        sync.RWMutex
	sprockets map[string]Sprocket
}

func NewMemorySprocketRepository() *MemorySprocketRepository {
	return &MemorySprocketRepository{
		sprockets: make(map[string]Sprocket),
	}
}

func (r *MemorySprocketRepository) Create(_ context.Context, sprocket Sprocket) (Sprocket, error) {
	id, err := newID()
	if err != nil {
		return Sprocket{}, err
	}

	sprocket.ID = id

	r.mu.Lock()
	defer r.mu.Unlock()
	r.sprockets[sprocket.ID] = sprocket

	return sprocket, nil
}

func (r *MemorySprocketRepository) Get(_ context.Context, id string) (Sprocket, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sprocket, ok := r.sprockets[id]
	if !ok {
		return Sprocket{}, ErrNotFound
	}

	return sprocket, nil
}

func (r *MemorySprocketRepository) List(_ context.Context) ([]Sprocket, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sprockets := make([]Sprocket, 0, len(r.sprockets))
	for _, sprocket := range r.sprockets {
		sprockets = append(sprockets, sprocket)
	}

	return sprockets, nil
}

func (r *MemorySprocketRepository) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.sprockets[id]; !ok {
		return ErrNotFound
	}

	delete(r.sprockets, id)
	return nil
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
