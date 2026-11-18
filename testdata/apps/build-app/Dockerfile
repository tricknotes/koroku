FROM busybox:1.37
COPY VERSION entrypoint.sh /app/
STOPSIGNAL SIGKILL
CMD ["sh", "/app/entrypoint.sh"]
