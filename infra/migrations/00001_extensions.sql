-- +goose Up
-- Extensions required by the canonical schema (specs/database/postgres.sql).
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
