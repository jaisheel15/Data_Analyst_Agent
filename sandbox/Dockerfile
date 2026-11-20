# Sandbox image: runs LLM-generated SQL/Python. No network at runtime, no pip installs.
FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 MPLBACKEND=Agg \
    MPLCONFIGDIR=/tmp/mpl HOME=/tmp

COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt \
 && useradd --uid 10001 --no-create-home --shell /usr/sbin/nologin sandbox \
 && mkdir -p /data /out /scratch

COPY runner.py reaper.py /app/

USER 10001
WORKDIR /tmp
# PID 1 idles and reaps orphans; the Go sandbox manager runs `python /app/runner.py` via exec.
CMD ["python", "/app/reaper.py"]
