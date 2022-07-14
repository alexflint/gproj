
list-credentials:
	sqlite3 ~/.config/gcloud/credentials.db 'select * from credentials'

list-tokens:
	sqlite3 ~/.config/gcloud/access_tokens.db 'select * from access_tokens'
