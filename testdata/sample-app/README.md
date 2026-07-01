# Ship Sample App

This fixture is intentionally tiny and CI-safe. The acceptance tests reference its Docker build context, but they do not require Docker by default.

- HTTP service: `/app/sample-app server`, listens on `PORT` or `3000`, and serves `/up`.
- Worker process: `/app/sample-app worker`.
- Health command: `/app/sample-app healthcheck`.
- Optional accessory dependency: set `DATABASE_URL` to simulate a database-backed deployment.
