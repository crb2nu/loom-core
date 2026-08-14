package service

import (
	"context"
	"errors"
	"strings"

	"github.com/crb2nu/sprocket/internal/repository"
)

var ErrValidation = errors.New("validation failed")

type Sprocket = repository.Sprocket

type CreateSprocketInput struct {
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
}

type SprocketService struct {
	repo repository.SprocketRepository
}

func NewSprocketService(repo repository.SprocketRepository) *SprocketService {
	return &SprocketService{repo: repo}
}

func (s *SprocketService) Create(ctx context.Context, input CreateSprocketInput) (Sprocket, error) {
	if strings.TrimSpace(input.Name) == "" {
		return Sprocket{}, ErrValidation
	}
	if input.Quantity < 0 {
		return Sprocket{}, ErrValidation
	}

	return s.repo.Create(ctx, Sprocket{
		Name:     input.Name,
		Quantity: input.Quantity,
	})
}

func (s *SprocketService) Get(ctx context.Context, id string) (Sprocket, error) {
	return s.repo.Get(ctx, id)
}

func (s *SprocketService) List(ctx context.Context) ([]Sprocket, error) {
	sprockets, err := s.repo.List(ctx)
	if sprockets == nil {
		sprockets = []Sprocket{}
	}
	return sprockets, err
}

func (s *SprocketService) Delete(ctx context.Context, id string) error {
	return s.repo.Delete(ctx, id)
}
