import json
import logging
from typing import Callable, Optional

from ..utils.ws_client import WebSocketClient
from .action_handlers import ActionHandlers
from .docker_operations import DockerManager
from .file_manager import FileManager
from .progress_reporter import ProgressReporter

logging.basicConfig(level=logging.INFO)


class BaseOTA:
    """Base OTA class for handling OTA operations."""

    def __init__(
        self, ota_server_url: str, om_api_key: str, om_api_key_id: str
    ) -> None:
        """
        Base OTA class for handling OTA operations.

        Parameters
        ----------
        ota_server_url : str
            The URL of the OTA server
        om_api_key : str
            The OpenMind API key
        om_api_key_id : str
            The OpenMind API key ID
        """
        self.ota_server_url = ota_server_url
        self.om_api_key = om_api_key
        self.om_api_key_id = om_api_key_id

        if not self.ota_server_url or not self.om_api_key or not self.om_api_key_id:
            raise ValueError("OTA server URL and API keys must be provided")

        self.progress_reporter = ProgressReporter()
        self.file_manager = FileManager()
        self.docker_manager = DockerManager(self.progress_reporter)
        self.action_handlers = ActionHandlers(
            self.docker_manager, self.progress_reporter, self.file_manager
        )

        self.ws_client = self.create_ws_client()
        self.progress_reporter.set_ws_client(self.ws_client)

        self.ota_process_callback: Optional[Callable] = None

    def create_ws_client(self) -> WebSocketClient:
        """
        Factory function to create a WebSocketClient instance.

        Returns
        -------
        WebSocketClient
            An instance of WebSocketClient
        """
        return WebSocketClient(
            url=f"{self.ota_server_url}?api_key_id={self.om_api_key_id}&api_key={self.om_api_key}"
        )

    def ota_process(self, message: str, ws_client=None):
        """
        Process OTA messages received from the WebSocket server.

        Parameters
        ----------
        message : str
            The message received from the WebSocket server.
        ws_client : WebSocketClient, optional
            WebSocket client for sending progress updates
        """
        if ws_client:
            self.progress_reporter.set_ws_client(ws_client)

        if isinstance(message, str):
            logging.info(f"Received OTA message: {message}")
            try:
                data = json.loads(message)
                logging.info(f"Processing OTA data: {data}")

                action = data.get("action")
                service_name = data.get("service_name")

                if not action:
                    logging.error("Invalid OTA message: missing action")
                    return

                if not service_name:
                    logging.error("Invalid OTA message: missing service_name")
                    return

                logging.info(f"Processing {action} action for service: {service_name}")

                if action == "upgrade":
                    self.action_handlers.handle_upgrade_action(data, service_name)
                elif action == "stop":
                    self.action_handlers.handle_stop_action(data, service_name)
                elif action == "start":
                    self.action_handlers.handle_start_action(data, service_name)
                elif action == "pause":
                    self.action_handlers.handle_pause_action(data, service_name)
                elif action == "unpause":
                    self.action_handlers.handle_unpause_action(data, service_name)
                elif action == "restart":
                    self.action_handlers.handle_restart_action(data, service_name)
                else:
                    error_msg = f"Unknown action type: {action}. Supported actions: upgrade, stop, start, pause, unpause, restart"
                    logging.error(error_msg)
                    self.progress_reporter.send_progress_update("error", error_msg, 0)

            except json.JSONDecodeError as e:
                logging.error(f"Failed to decode JSON message: {e}")
                self.progress_reporter.send_progress_update(
                    "decode_error", "Failed to decode message", 0
                )

            if self.ota_process_callback:
                self.ota_process_callback(message)

        else:
            logging.warning("Received non-string message, ignoring.")
            self.progress_reporter.send_progress_update(
                "error_message", "Failed to decode message", 0
            )

    def set_ota_process_callback(self, callback: Callable):
        """
        Set a callback function to be called after processing an OTA message.

        Parameters
        ----------
        callback : Callable
            The callback function to set
        """
        self.ota_process_callback = callback
