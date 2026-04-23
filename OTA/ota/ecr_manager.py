import json
import logging
import threading

from .docker_operations import DockerManager
from .progress_reporter import ProgressReporter

logging.basicConfig(level=logging.INFO)


class ECRManager:
    """Manages ECR credential requests and docker login for private images."""

    def __init__(
        self,
        docker_manager: DockerManager,
        progress_reporter: ProgressReporter,
    ):
        self.docker_manager = docker_manager
        self.progress_reporter = progress_reporter
        self._ws_send = None
        self._event = threading.Event()
        self._credentials: dict | None = None
        self._error: str | None = None

    def set_ws_send(self, ws_send):
        """Set the WebSocket send function for credential requests."""
        self._ws_send = ws_send

    def is_private_ecr(self, yaml_content: dict) -> str | None:
        """Return ECR repo name if any service uses a private ECR image, else None."""
        for svc in yaml_content.get("services", {}).values():
            image = svc.get("image", "")
            if ".dkr.ecr." in image:
                repo = image
                slash_idx = repo.find("/")
                if slash_idx > 0 and "." in repo[:slash_idx]:
                    repo = repo[slash_idx + 1 :]
                colon_idx = repo.find(":")
                if colon_idx > 0:
                    repo = repo[:colon_idx]
                return repo
        return None

    def login_if_needed(self, yaml_content: dict) -> bool:
        """Check for private ECR image and login if needed. Returns True on success or no ECR."""
        ecr_image = self.is_private_ecr(yaml_content)
        if not ecr_image:
            return True
        return self._request_credentials_and_login(ecr_image)

    def _request_credentials_and_login(self, image: str) -> bool:
        """Send WS request, block until credentials received, docker login."""
        if not self._ws_send:
            logging.error("No WS send function configured for ECR")
            return False

        self._event.clear()
        self._credentials = None
        self._error = None

        self.progress_reporter.send_progress_update(
            "authenticating", "Requesting ECR credentials", 15
        )
        self._ws_send(json.dumps({"type": "ecr_credentials_request", "image": image}))
        logging.info(f"Sent ECR credentials request for: {image}")

        if not self._event.wait(timeout=60):
            logging.error("ECR credentials request timed out")
            self.progress_reporter.send_progress_update(
                "error", "ECR credentials request timed out", 15
            )
            return False

        if self._error:
            logging.error(f"ECR credentials error: {self._error}")
            self.progress_reporter.send_progress_update(
                "error", f"ECR credentials error: {self._error}", 15
            )
            return False

        creds = self._credentials
        login_ok = self.docker_manager.login_docker_ecr(
            registry=creds.get("registry", ""),
            username=creds.get("username", ""),
            password=creds.get("password", ""),
        )
        if not login_ok:
            logging.error("Docker ECR login failed")
            self.progress_reporter.send_progress_update(
                "error", "Docker ECR login failed", 15
            )
            return False

        logging.info(f"ECR login succeeded, expires at {creds.get('expires_at')}")
        return True

    def on_credentials_received(self, credentials: dict):
        """Called when ECR credentials response arrives via WS."""
        self._credentials = credentials
        self._event.set()

    def on_credentials_error(self, error: str):
        """Called when ECR credentials error arrives via WS."""
        self._error = error
        self._event.set()
