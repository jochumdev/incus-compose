leafwiki:
    leafwiki --data-dir="docs" --jwt-secret="none" --admin-password="insecure" --allow-insecure --disable-auth

fix-docs:
    npx --yes prettier --write .
    cd docs; npx --yes prettier --write .
