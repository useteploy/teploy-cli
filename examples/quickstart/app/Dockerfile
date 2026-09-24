FROM busybox:1.37
COPY index.html /www/index.html
# teploy injects PORT (the published port) as an env var; listen there so
# the health gate and the published port see the same listener.
CMD ["sh", "-c", "httpd -f -p ${PORT:-80} -h /www"]
