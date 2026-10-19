Remote connections are blocked until you allow a network.

1. Make yourself an admin: set `MARIADB_ADMINS="<your username>"` in `/app/data/env.sh` and restart the app, or add yourself to a Cloudron group named `mariadb-admins`.
2. Open the app, sign in, and go to **Admin**.
3. Add the IP address of your client server under **Admin > Network**.
4. Create a role under **Admin > Roles** and choose its owners (Cloudron users and groups).
5. Each owner signs in, opens the role settings (gear icon) on **My databases** and creates an own password. It is shown once and not stored.
6. Connect with TLS verification, using your login `<role>.<username>`:
   `mariadb --host=$CLOUDRON-APP-FQDN --port=<port> --user=<role>.<username> --password --ssl-verify-server-cert <database>`
   With the MySQL client use `--ssl-mode=VERIFY_IDENTITY`. If your system has no Let's Encrypt root certificate, download [isrgrootx1.pem](https://letsencrypt.org/certs/isrgrootx1.pem) and pass it with `--ssl-ca`.
