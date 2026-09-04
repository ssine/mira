.PHONY: mira-objects
mira-objects:
	@echo 'ssh|$(SSHOBJS)'
	@echo 'sshd|$(SSHDOBJS)'
	@echo 'sshd-session|$(SSHD_SESSION_OBJS)'
	@echo 'sshd-auth|$(SSHD_AUTH_OBJS)'
	@echo 'scp|$(SCP_OBJS)'
	@echo 'sftp|$(SFTP_OBJS)'
	@echo 'sftp-server|$(SFTPSERVER_OBJS)'
	@echo 'ssh-keygen|ssh-keygen.o sshsig.o $(P11OBJS) $(SKOBJS)'
