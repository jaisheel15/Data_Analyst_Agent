FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.11.7 /uv /usr/local/bin/uv
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy PYTHONUNBUFFERED=1
WORKDIR /srv
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project
COPY app ./app
RUN useradd --uid 10003 --no-create-home agent
USER 10003
EXPOSE 8000
CMD ["/srv/.venv/bin/uvicorn", "--factory", "app.main:app_from_env", "--host", "0.0.0.0", "--port", "8000"]
