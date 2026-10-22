# fakesnow in server mode: a local fake of Snowflake's REST API, backed by
# DuckDB (https://github.com/tekumara/fakesnow). The integration tests
# (-tags integration) build it with testcontainers. Every version is
# pinned: snowflake-connector-python 5 removed a module fakesnow 0.11.22
# imports. It is not Snowflake; see the driver README for what was tested
# where.
FROM python:3.13.9-slim
RUN pip install --no-cache-dir "fakesnow[server]==0.11.22" "snowflake-connector-python==4.8.0"
EXPOSE 8080
ENTRYPOINT ["python", "-c", "import uvicorn; from fakesnow.server import app; uvicorn.run(app, host='0.0.0.0', port=8080, log_level='info', access_log=False)"]
