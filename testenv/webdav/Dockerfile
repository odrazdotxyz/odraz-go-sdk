FROM httpd:2.4
RUN apt-get update -qq && apt-get install -y -qq apache2-utils >/dev/null && rm -rf /var/lib/apt/lists/* \
 && htpasswd -bc /usr/local/apache2/.htpasswd odraz odraz-test
COPY httpd.conf /usr/local/apache2/conf/httpd.conf
