FROM nginx:1.27-alpine
ARG DEMO_VERSION=stable
COPY index.html /usr/share/nginx/html/index.template.html
RUN sed "s/__FRONTEND_VERSION__/${DEMO_VERSION}/g" /usr/share/nginx/html/index.template.html > /usr/share/nginx/html/index.html \
    && rm /usr/share/nginx/html/index.template.html
EXPOSE 80
