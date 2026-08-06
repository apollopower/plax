#!/bin/bash
psql "$DATABASE_URL" -c "INSERT INTO users (name) VALUES ('seed-user');"
