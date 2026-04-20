import logging
import os
import re
import subprocess
import tempfile

import yaml

logging.basicConfig(level=logging.INFO)


class DockerManager:
    """Handles all Docker-related operations for OTA updates."""

    def __init__(self, progress_reporter=None):
        """
        Initialize DockerManager.

        Parameters
        ----------
        progress_reporter : ProgressReporter, optional
            Reporter for sending progress updates
        """
        self.progress_reporter = progress_reporter
        self._completed_layers = set()

    def login_docker_ecr(self, registry: str, username: str, password: str) -> bool:
        """Login to an ECR private registry for pulling images.

        Parameters
        ----------
        registry : str
            ECR registry URL
        username : str
            Docker username
        password : str
            Temporary Docker password

        Returns
        -------
        bool
            True if login succeeded
        """
        if not all([registry, username, password]):
            logging.error("ECR login failed: missing required parameters")
            return False

        try:
            cmd = ["docker", "login", "--username", username, "--password-stdin", registry]
            result = subprocess.run(
                cmd, input=password, capture_output=True, text=True, timeout=30
            )
            if result.returncode == 0:
                logging.info(f"ECR login successful: {registry}")
                return True
            else:
                logging.error(f"ECR login failed for registry: {registry}")
                return False
        except subprocess.TimeoutExpired:
            logging.error("ECR login timed out")
            return False
        except Exception as e:
            logging.error(f"ECR login error: {e}")
            return False

    def stop_docker_services(self, yaml_content: dict) -> dict:
        """
        Stop Docker containers/services based on the update configuration.

        Parameters
        ----------
        yaml_content : dict
            The parsed YAML content containing service definitions

        Returns
        -------
        dict
            Result with success status and error information
        """
        try:
            services = yaml_content.get("services", {})

            if not services:
                logging.warning("No services defined in update YAML")
                return {"success": True, "message": "No services to stop"}

            stopped_services = []
            failed_services = []

            for service_name, service_config in services.items():
                try:
                    container_name = service_config.get("container_name", service_name)

                    check_cmd = [
                        "docker",
                        "ps",
                        "-q",
                        "--filter",
                        f"name={container_name}",
                    ]
                    check_result = subprocess.run(
                        check_cmd, capture_output=True, text=True, timeout=10
                    )

                    if check_result.returncode == 0 and check_result.stdout.strip():
                        stop_cmd = ["docker", "stop", container_name]
                        stop_result = subprocess.run(
                            stop_cmd, capture_output=True, text=True, timeout=30
                        )

                        if stop_result.returncode == 0:
                            logging.info(f"Stopped container: {container_name}")

                            remove_cmd = ["docker", "rm", container_name]
                            remove_result = subprocess.run(
                                remove_cmd, capture_output=True, text=True, timeout=10
                            )

                            if remove_result.returncode == 0:
                                logging.info(f"Removed container: {container_name}")
                                stopped_services.append(container_name)
                            else:
                                logging.warning(
                                    f"Normal remove failed for {container_name}, trying force remove"
                                )
                                force_remove_cmd = [
                                    "docker",
                                    "rm",
                                    "-f",
                                    container_name,
                                ]
                                force_remove_result = subprocess.run(
                                    force_remove_cmd,
                                    capture_output=True,
                                    text=True,
                                    timeout=10,
                                )

                                if force_remove_result.returncode == 0:
                                    logging.info(
                                        f"Force removed container: {container_name}"
                                    )
                                    stopped_services.append(container_name)
                                else:
                                    logging.error(
                                        f"Failed to remove container {container_name}: {force_remove_result.stderr}"
                                    )
                                    failed_services.append(container_name)
                        else:
                            logging.warning(
                                f"Normal stop failed for {container_name}, trying force stop"
                            )
                            force_stop_cmd = ["docker", "kill", container_name]
                            force_stop_result = subprocess.run(
                                force_stop_cmd,
                                capture_output=True,
                                text=True,
                                timeout=10,
                            )

                            if force_stop_result.returncode == 0:
                                logging.info(
                                    f"Force stopped container: {container_name}"
                                )

                                remove_cmd = ["docker", "rm", "-f", container_name]
                                remove_result = subprocess.run(
                                    remove_cmd,
                                    capture_output=True,
                                    text=True,
                                    timeout=10,
                                )

                                if remove_result.returncode == 0:
                                    logging.info(
                                        f"Removed container after force stop: {container_name}"
                                    )
                                    stopped_services.append(container_name)
                                else:
                                    logging.error(
                                        f"Failed to remove container after force stop {container_name}: {remove_result.stderr}"
                                    )
                                    failed_services.append(container_name)
                            else:
                                stderr_output = force_stop_result.stderr
                                if "No such container" in stderr_output:
                                    logging.info(
                                        f"Container {container_name} no longer exists, considering as stopped"
                                    )
                                    stopped_services.append(container_name)
                                else:
                                    logging.error(
                                        f"Failed to force stop container {container_name}: {stderr_output}"
                                    )
                                    failed_services.append(container_name)
                    else:
                        check_stopped_cmd = [
                            "docker",
                            "ps",
                            "-a",
                            "-q",
                            "--filter",
                            f"name={container_name}",
                        ]
                        check_stopped_result = subprocess.run(
                            check_stopped_cmd,
                            capture_output=True,
                            text=True,
                            timeout=10,
                        )

                        if (
                            check_stopped_result.returncode == 0
                            and check_stopped_result.stdout.strip()
                        ):
                            remove_cmd = ["docker", "rm", "-f", container_name]
                            remove_result = subprocess.run(
                                remove_cmd, capture_output=True, text=True, timeout=10
                            )

                            if remove_result.returncode == 0:
                                logging.info(
                                    f"Removed stopped container: {container_name}"
                                )
                            else:
                                logging.warning(
                                    f"Failed to remove stopped container {container_name}: {remove_result.stderr}"
                                )
                        else:
                            logging.info(
                                f"Container {container_name} not found, considering as already stopped"
                            )
                            stopped_services.append(service_name)

                except subprocess.TimeoutExpired:
                    logging.error(f"Timeout stopping service {service_name}")
                    failed_services.append(service_name)
                except Exception as e:
                    logging.error(f"Error stopping service {service_name}: {e}")
                    failed_services.append(service_name)

            if failed_services:
                return {
                    "success": False,
                    "error": f"Failed to stop services: {', '.join(failed_services)}",
                    "stopped": stopped_services,
                    "failed": failed_services,
                }

            return {
                "success": True,
                "message": f"Successfully stopped {len(stopped_services)} services",
                "stopped": stopped_services,
            }

        except Exception as e:
            logging.error(f"Error in stop_docker_services: {e}")
            return {"success": False, "error": str(e)}

    def pull_images_with_progress(self, pull_cmd: list) -> dict:
        """
        Pull Docker images with real-time progress reporting.

        Parameters
        ----------
        pull_cmd : list
            The docker-compose pull command

        Returns
        -------
        dict
            Result with success status and output information
        """
        try:
            self._completed_layers = set()

            process = subprocess.Popen(
                pull_cmd,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                bufsize=1,
                universal_newlines=True,
            )

            stdout_lines = []
            services_found = set()
            services_pulled = set()
            current_service = None

            while True:
                if process.stdout is None:
                    break
                output = process.stdout.readline()
                if output == "" and process.poll() is not None:
                    break

                if output:
                    stdout_lines.append(output.strip())
                    line = output.strip()
                    logging.info(f"Docker pull output: {line}")

                    if line.startswith("Pulling "):
                        service_match = re.search(r"^Pulling\s+(\w+)", line)
                        if service_match:
                            current_service = service_match.group(1)
                            services_found.add(current_service)
                            progress = 30 + (
                                len(services_pulled) * 40 // max(len(services_found), 1)
                            )
                            self._send_progress_update(
                                "pulling_service",
                                f"Pulling {current_service}",
                                progress,
                            )

                    elif "Pull complete" in line:
                        layer_match = re.search(r"^([a-f0-9]+)\s+Pull complete", line)
                        if layer_match:
                            layer_id = layer_match.group(1)
                            if not hasattr(self, "_completed_layers"):
                                self._completed_layers = set()
                            self._completed_layers.add(layer_id)

                            progress = 30 + min(len(self._completed_layers) * 5, 40)
                            self._send_progress_update(
                                "layer_complete",
                                f"Completed layer {layer_id[:12]}...",
                                progress,
                            )

                    elif "Downloading" in line:
                        download_match = re.search(
                            r"^([a-f0-9]+)\s+Downloading.*?(\d+(?:\.\d+)?[KMGT]?B)/(\d+(?:\.\d+)?[KMGT]?B)",
                            line,
                        )
                        if download_match:
                            layer_id, current_size, total_size = download_match.groups()
                            progress = 35 + (
                                len(services_pulled)
                                * 35
                                // max(len(services_found) or 1, 1)
                            )
                            self._send_progress_update(
                                "downloading",
                                f"Downloading layer {layer_id[:12]}...: {current_size}/{total_size}",
                                progress,
                            )

                    elif "Extracting" in line:
                        extract_match = re.search(r"^([a-f0-9]+)\s+Extracting", line)
                        if extract_match:
                            layer_id = extract_match.group(1)
                            progress = 50 + (
                                len(services_pulled)
                                * 20
                                // max(len(services_found) or 1, 1)
                            )
                            self._send_progress_update(
                                "extracting",
                                f"Extracting layer {layer_id[:12]}...",
                                progress,
                            )

            stdout_remainder, stderr_output = process.communicate()
            if stdout_remainder:
                stdout_lines.extend(stdout_remainder.strip().split("\n"))

            return_code = (
                process.returncode
                if process.returncode is not None
                else process.wait(timeout=900)
            )

            stdout_text = "\n".join(stdout_lines)
            stderr_text = stderr_output or ""

            if return_code == 0:
                logging.info("Successfully pulled all Docker images")
                return {
                    "success": True,
                    "message": "Successfully pulled all images",
                    "output": stdout_text,
                    "services_pulled": list(services_pulled),
                }
            else:
                error_msg = f"Pull failed with return code {return_code}: {stderr_text}"
                logging.error(error_msg)
                return {
                    "success": False,
                    "error": error_msg,
                    "output": stdout_text,
                    "stderr": stderr_text,
                }

        except subprocess.TimeoutExpired:
            process.kill()
            error_msg = "Docker pull operation timed out"
            logging.error(error_msg)
            return {
                "success": False,
                "error": error_msg,
                "output": "\n".join(stdout_lines) if "stdout_lines" in locals() else "",
            }
        except Exception as e:
            error_msg = f"Error during pull operation: {e}"
            logging.error(error_msg)
            return {
                "success": False,
                "error": error_msg,
                "output": "\n".join(stdout_lines) if "stdout_lines" in locals() else "",
            }

    def start_docker_services(self, yaml_content: dict) -> dict:
        """
        Start Docker containers/services based on the update configuration.

        Parameters
        ----------
        yaml_content : dict
            The parsed YAML content containing service definitions

        Returns
        -------
        dict
            Result with success status and error information
        """
        try:
            services = yaml_content.get("services", {})

            if not services:
                logging.warning("No services defined in update YAML")
                return {"success": True, "message": "No services to start"}

            temp_compose_file = None
            try:
                temp_compose_file = self.create_temp_compose_file(yaml_content)

                logging.info("Pulling Docker images...")
                self._send_progress_update(
                    "pulling", "Starting to pull Docker images", 30
                )

                pull_cmd = ["docker-compose", "-f", temp_compose_file, "pull"]
                pull_result = self.pull_images_with_progress(pull_cmd)

                if not pull_result.get("success"):
                    return {
                        "success": False,
                        "error": f"docker-compose pull failed: {pull_result.get('error')}",
                        "output": pull_result.get("output", ""),
                    }

                logging.info("Successfully pulled Docker images")
                self._send_progress_update(
                    "pulled", "Successfully pulled Docker images", 70
                )

                logging.info("Starting Docker services...")
                self._send_progress_update(
                    "starting_services", "Starting Docker services", 80
                )
                up_cmd = [
                    "docker-compose",
                    "-f",
                    temp_compose_file,
                    "up",
                    "-d",
                    "--no-build",
                ]
                up_result = subprocess.run(
                    up_cmd,
                    capture_output=True,
                    text=True,
                    timeout=120,  # 2 minutes timeout for starting
                )

                if up_result.returncode == 0:
                    logging.info("Successfully started services with docker-compose")

                    # Clean up old images after successful deployment
                    cleanup_result = self.cleanup_old_images()

                    return {
                        "success": True,
                        "message": "Successfully pulled images and started updated services",
                        "pull_output": pull_result.get("output", ""),
                        "up_output": up_result.stdout,
                        "cleanup_result": cleanup_result,
                    }
                else:
                    logging.error(f"Failed to start services: {up_result.stderr}")
                    return {
                        "success": False,
                        "error": f"docker-compose up failed: {up_result.stderr}",
                        "pull_output": pull_result.get("output", ""),
                        "up_output": up_result.stdout,
                    }

            finally:
                if temp_compose_file and os.path.exists(temp_compose_file):
                    try:
                        os.unlink(temp_compose_file)
                    except OSError as e:
                        logging.warning(
                            f"Failed to clean up temporary compose file: {e}"
                        )

        except subprocess.TimeoutExpired:
            logging.error("Timeout starting Docker services")
            return {"success": False, "error": "Timeout starting services"}
        except Exception as e:
            logging.error(f"Error in start_docker_services: {e}")
            return {"success": False, "error": str(e)}

    def create_temp_compose_file(self, yaml_content: dict) -> str:
        """
        Create a temporary docker-compose file from YAML content.

        Parameters
        ----------
        yaml_content : dict
            The parsed YAML content

        Returns
        -------
        str
            Path to the temporary docker-compose file
        """
        temp_file = tempfile.NamedTemporaryFile(mode="w", suffix=".yml", delete=False)
        try:
            yaml.dump(yaml_content, temp_file, default_flow_style=False)
            temp_file.flush()
            logging.info(f"Created temporary docker-compose file: {temp_file.name}")
            return temp_file.name
        finally:
            temp_file.close()

    def cleanup_old_images(self) -> dict:
        """
        Clean up old, unused Docker images to free up disk space.

        Returns
        -------
        dict
            Result with success status and cleanup information
        """
        try:
            logging.info("Cleaning up old Docker images...")
            self._send_progress_update("cleanup", "Cleaning up old Docker images", 90)

            cleanup_cmds = [
                ["docker", "image", "prune", "-f"],
                ["docker", "container", "prune", "-f"],
                ["docker", "system", "prune", "-f"],
            ]

            cleanup_results = []
            total_space_freed = "0B"

            for cmd_name, cmd in zip(["images", "containers", "system"], cleanup_cmds):
                try:
                    result = subprocess.run(
                        cmd, capture_output=True, text=True, timeout=60
                    )

                    if result.returncode == 0:
                        output = result.stdout.strip()
                        cleanup_results.append(
                            {"command": cmd_name, "success": True, "output": output}
                        )

                        if "Total reclaimed space:" in output:
                            space_match = re.search(
                                r"Total reclaimed space:\s*([\d.]+\w+)", output
                            )
                            if space_match:
                                total_space_freed = space_match.group(1)

                        logging.info(f"Successfully cleaned up {cmd_name}: {output}")
                    else:
                        cleanup_results.append(
                            {
                                "command": cmd_name,
                                "success": False,
                                "error": result.stderr,
                            }
                        )
                        logging.warning(
                            f"Failed to cleanup {cmd_name}: {result.stderr}"
                        )

                except subprocess.TimeoutExpired:
                    cleanup_results.append(
                        {
                            "command": cmd_name,
                            "success": False,
                            "error": "Cleanup timeout",
                        }
                    )
                    logging.warning(f"Timeout during {cmd_name} cleanup")
                except Exception as e:
                    cleanup_results.append(
                        {"command": cmd_name, "success": False, "error": str(e)}
                    )
                    logging.warning(f"Error during {cmd_name} cleanup: {e}")

            successful_cleanups = [r for r in cleanup_results if r["success"]]

            if successful_cleanups:
                message = (
                    f"Cleaned up Docker resources. Space freed: {total_space_freed}"
                )
                logging.info(message)
                self._send_progress_update("cleanup_complete", message, 95)
                return {
                    "success": True,
                    "message": message,
                    "space_freed": total_space_freed,
                    "cleanup_results": cleanup_results,
                }
            else:
                message = "Cleanup completed but no space was freed"
                logging.info(message)
                return {
                    "success": True,
                    "message": message,
                    "space_freed": "0B",
                    "cleanup_results": cleanup_results,
                }

        except Exception as e:
            error_msg = f"Error during Docker cleanup: {e}"
            logging.error(error_msg)
            self._send_progress_update("cleanup_error", error_msg, 90)
            return {"success": False, "error": error_msg}

    def pause_docker_services(self, yaml_content: dict) -> dict:
        """
        Pause Docker containers based on the service configuration.

        Parameters
        ----------
        yaml_content : dict
            The parsed YAML content containing service definitions

        Returns
        -------
        dict
            Result with success status and error information
        """
        try:
            services = yaml_content.get("services", {})

            if not services:
                logging.warning("No services defined in YAML")
                return {"success": True, "message": "No services to pause"}

            paused_services = []
            failed_services = []

            for service_name, service_config in services.items():
                try:
                    container_name = service_config.get("container_name", service_name)

                    check_cmd = [
                        "docker",
                        "ps",
                        "-q",
                        "--filter",
                        f"name={container_name}",
                    ]
                    check_result = subprocess.run(
                        check_cmd, capture_output=True, text=True, timeout=10
                    )

                    if check_result.returncode == 0 and check_result.stdout.strip():
                        pause_cmd = ["docker", "pause", container_name]
                        pause_result = subprocess.run(
                            pause_cmd, capture_output=True, text=True, timeout=30
                        )

                        if pause_result.returncode == 0:
                            logging.info(f"Paused container: {container_name}")
                            paused_services.append(container_name)
                        else:
                            logging.error(
                                f"Failed to pause container {container_name}: {pause_result.stderr}"
                            )
                            failed_services.append(container_name)
                    else:
                        logging.info(f"Container {container_name} is not running")

                except subprocess.TimeoutExpired:
                    logging.error(f"Timeout pausing service {service_name}")
                    failed_services.append(service_name)
                except Exception as e:
                    logging.error(f"Error pausing service {service_name}: {e}")
                    failed_services.append(service_name)

            if failed_services:
                return {
                    "success": False,
                    "error": f"Failed to pause services: {', '.join(failed_services)}",
                    "paused": paused_services,
                    "failed": failed_services,
                }

            return {
                "success": True,
                "message": f"Successfully paused {len(paused_services)} services",
                "paused": paused_services,
            }

        except Exception as e:
            logging.error(f"Error in pause_docker_services: {e}")
            return {"success": False, "error": str(e)}

    def unpause_docker_services(self, yaml_content: dict) -> dict:
        """
        Unpause Docker containers based on the service configuration.

        Parameters
        ----------
        yaml_content : dict
            The parsed YAML content containing service definitions

        Returns
        -------
        dict
            Result with success status and error information
        """
        try:
            services = yaml_content.get("services", {})

            if not services:
                logging.warning("No services defined in YAML")
                return {"success": True, "message": "No services to unpause"}

            unpaused_services = []
            failed_services = []

            for service_name, service_config in services.items():
                try:
                    container_name = service_config.get("container_name", service_name)

                    check_cmd = [
                        "docker",
                        "ps",
                        "-a",
                        "-q",
                        "--filter",
                        f"name={container_name}",
                        "--filter",
                        "status=paused",
                    ]
                    check_result = subprocess.run(
                        check_cmd, capture_output=True, text=True, timeout=10
                    )

                    if check_result.returncode == 0 and check_result.stdout.strip():
                        unpause_cmd = ["docker", "unpause", container_name]
                        unpause_result = subprocess.run(
                            unpause_cmd, capture_output=True, text=True, timeout=30
                        )

                        if unpause_result.returncode == 0:
                            logging.info(f"Unpaused container: {container_name}")
                            unpaused_services.append(container_name)
                        else:
                            logging.error(
                                f"Failed to unpause container {container_name}: {unpause_result.stderr}"
                            )
                            failed_services.append(container_name)
                    else:
                        logging.info(f"Container {container_name} is not paused")

                except subprocess.TimeoutExpired:
                    logging.error(f"Timeout unpausing service {service_name}")
                    failed_services.append(service_name)
                except Exception as e:
                    logging.error(f"Error unpausing service {service_name}: {e}")
                    failed_services.append(service_name)

            if failed_services:
                return {
                    "success": False,
                    "error": f"Failed to unpause services: {', '.join(failed_services)}",
                    "unpaused": unpaused_services,
                    "failed": failed_services,
                }

            return {
                "success": True,
                "message": f"Successfully unpaused {len(unpaused_services)} services",
                "unpaused": unpaused_services,
            }

        except Exception as e:
            logging.error(f"Error in unpause_docker_services: {e}")
            return {"success": False, "error": str(e)}

    def restart_docker_services(self, yaml_content: dict) -> dict:
        """
        Restart Docker containers based on the service configuration.

        Parameters
        ----------
        yaml_content : dict
            The parsed YAML content containing service definitions

        Returns
        -------
        dict
            Result with success status and error information
        """
        try:
            services = yaml_content.get("services", {})

            if not services:
                logging.warning("No services defined in YAML")
                return {"success": True, "message": "No services to restart"}

            restarted_services = []
            failed_services = []

            for service_name, service_config in services.items():
                try:
                    container_name = service_config.get("container_name", service_name)

                    check_cmd = [
                        "docker",
                        "ps",
                        "-a",
                        "-q",
                        "--filter",
                        f"name={container_name}",
                    ]
                    check_result = subprocess.run(
                        check_cmd, capture_output=True, text=True, timeout=10
                    )

                    if check_result.returncode == 0 and check_result.stdout.strip():
                        restart_cmd = ["docker", "restart", container_name]
                        restart_result = subprocess.run(
                            restart_cmd, capture_output=True, text=True, timeout=60
                        )

                        if restart_result.returncode == 0:
                            logging.info(f"Restarted container: {container_name}")
                            restarted_services.append(container_name)
                        else:
                            logging.error(
                                f"Failed to restart container {container_name}: {restart_result.stderr}"
                            )
                            failed_services.append(container_name)
                    else:
                        logging.info(f"Container {container_name} not found")

                except subprocess.TimeoutExpired:
                    logging.error(f"Timeout restarting service {service_name}")
                    failed_services.append(service_name)
                except Exception as e:
                    logging.error(f"Error restarting service {service_name}: {e}")
                    failed_services.append(service_name)

            if failed_services:
                return {
                    "success": False,
                    "error": f"Failed to restart services: {', '.join(failed_services)}",
                    "restarted": restarted_services,
                    "failed": failed_services,
                }

            return {
                "success": True,
                "message": f"Successfully restarted {len(restarted_services)} services",
                "restarted": restarted_services,
            }

        except Exception as e:
            logging.error(f"Error in restart_docker_services: {e}")
            return {"success": False, "error": str(e)}

    def _send_progress_update(self, status: str, message: str, progress: int):
        """
        Send progress update through the progress reporter.

        Parameters
        ----------
        status : str
            Current status of the operation
        message : str
            Progress message
        progress : int
            Progress percentage (0-100)
        """
        if self.progress_reporter:
            self.progress_reporter.send_progress_update(status, message, progress)
