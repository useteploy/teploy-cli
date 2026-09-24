FROM busybox:1.37
COPY index.html /www/index.html
# /health answers 200 so `teploy health` (HTTP first) passes on the real
# endpoint, not the 404->TCP fallback.
RUN echo ok > /www/health
# teploy injects PORT (the published port) as an env var; listen there so
# the health gate and the published port see the same listener. PID 1 gets
# no default signal handling, so the explicit TERM trap is what lets docker
# stop retire the container at once instead of waiting out the kill timeout.
CMD ["sh", "-c", "trap 'exit 0' TERM; httpd -f -p ${PORT:-80} -h /www & wait"]
