using System.Text.Json;
using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// Under `aspire run` the edge runs too: the Envoy ProtoStrap runs in an account, configured
/// by the same renderer, `protostrap edge render`, from the same routes, hosts and
/// authorizers, but reaching services where the AppHost runs them (docs/protostrap-aspire.md,
/// "The local edge").
/// </summary>
public class LocalEdgeTests
{
    [Fact]
    public void RunModeAddsTheLocalEdgeForTheHosts()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.dev.localhost");

        var edge = Assert.Single(builder.Resources.OfType<ContainerResource>(), r => r.Name == "protostrap-edge");
        var image = Assert.Single(edge.Annotations.OfType<ContainerImageAnnotation>());
        Assert.Equal(("envoyproxy/envoy", "v1.40.2"), (image.Image, image.Tag));
        var http = Assert.Single(edge.Annotations.OfType<EndpointAnnotation>());
        Assert.Equal((20000, 10000, false), (http.Port, http.TargetPort, http.IsProxied));
        var mount = Assert.Single(edge.Annotations.OfType<ContainerMountAnnotation>());
        Assert.Equal((repo.PathOf("apphost/obj/protostrap-edge"), "/etc/protostrap", true), (mount.Source, mount.Target, mount.IsReadOnly));
        Assert.True(edge.IsExcludedFromPublish());
    }

    [Fact]
    public async Task WithLocalEdgeMovesItsPortAndTheOrigins()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        var protostrap = builder.AddEnvironment()
            .WithHost("app", "protofast.dev", local: "app.dev.localhost")
            .WithLocalEdge(port: 21000);
        var api = builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"));

        var edge = Assert.Single(builder.Resources.OfType<ContainerResource>(), r => r.Name == "protostrap-edge");
        Assert.Equal(21000, Assert.Single(edge.Annotations.OfType<EndpointAnnotation>()).Port);
        ProtoStrapEnvironmentExtensions.AddHostVariables(builder.Resources, protostrap.Resource);
        var env = await RunModeEnvironmentAsync(api.Resource);
        Assert.Equal("app.dev.localhost", env["HOST_APP"]);
        Assert.Equal("http://app.dev.localhost:21000", env["ORIGIN_APP"]);
    }

    [Fact]
    public void PublishModeHasNoLocalEdge()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.dev.localhost");

        Assert.DoesNotContain(builder.Resources, r => r.Name == "protostrap-edge");
    }

    [Fact]
    public async Task TheLocalEdgeRoutesWhatTheSpecRoutes()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/keycloak/Dockerfile", "FROM quay.io/keycloak/keycloak:26.7\n");
        var builder = TestApp.Run(repo);
        var protostrap = builder.AddEnvironment()
            .WithHost("app", "protofast.dev", local: "app.dev.localhost")
            .WithHost("auth", "auth.protofast.dev", local: "auth.dev.localhost");
        var auth = builder.AddProject("auth", repo.Project("services/auth/Auth.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(name: "http")
            .WithHttpHealthCheck("/healthz")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi"));
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc)
                .Route("app/api/", rate: 50).Authorize(auth, path: "/auth/check", headers: ["x-user"], forward: ["cookie"], respond: ["set-cookie"]));
        builder.AddDockerfile("keycloak", "../services/keycloak")
            .WithHttpEndpoint(targetPort: 8080, name: "http")
            .WithHttpEndpoint(targetPort: 9000, name: "management")
            .PublishAsProtoStrapService(s => s.Request(cpu: "1000m", memory: "1Gi")
                .Ready(Readiness.Http("/health/ready").On(9000)).Routes("auth/realms/", "auth/resources/"));
        builder.AddProject("worker", repo.Project("services/worker/Worker.csproj"))
            .PublishAsProtoStrapWorker(w => w.Request(cpu: "250m", memory: "256Mi"));

        // The AppHost resolves each endpoint as a container on its network reaches it: a
        // project on the host, a container by its name and target port.
        Task<string> Resolve(IResource resource, EndpointAnnotation endpoint) => Task.FromResult(resource switch
        {
            ProjectResource => $"host.docker.internal:{(resource.Name == "api" ? 51234 : 51300)}",
            _ => $"{resource.Name}:{endpoint.TargetPort}",
        });
        var routing = await LocalEdge.RoutingAsync(builder.Resources, protostrap.Resource, 10000, Resolve, CancellationToken.None);

        Assert.Equal("""
            {
              "services": [
                {
                  "name": "api",
                  "port": 51234,
                  "ready": "grpc",
                  "protocol": "grpc",
                  "routes": [
                    {
                      "host": "app",
                      "path": "/api/",
                      "rate": 50
                    }
                  ],
                  "authorize": {
                    "by": "auth",
                    "path": "/auth/check",
                    "headers": [
                      "x-user"
                    ],
                    "forward": [
                      "cookie"
                    ],
                    "respond": [
                      "set-cookie"
                    ]
                  },
                  "address": "host.docker.internal:51234"
                },
                {
                  "name": "auth",
                  "port": 51300,
                  "ready": "/healthz",
                  "protocol": "http1",
                  "address": "host.docker.internal:51300"
                },
                {
                  "name": "keycloak",
                  "port": 8080,
                  "ready": "9000/health/ready",
                  "protocol": "http1",
                  "routes": [
                    {
                      "host": "auth",
                      "path": "/realms/"
                    },
                    {
                      "host": "auth",
                      "path": "/resources/"
                    }
                  ],
                  "address": "keycloak:8080",
                  "health_port": 9000
                }
              ],
              "hosts": {
                "app": "app.dev.localhost",
                "auth": "auth.dev.localhost"
              },
              "listen": "0.0.0.0:10000",
              "local": true
            }
            """, routing);
    }

    [Fact]
    public async Task AnAuthorizerWithNothingToRespondWritesNoRespond()
    {
        // The routing names only what the AppHost gives, so the edge renders as it did before
        // respond.
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        var protostrap = builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.dev.localhost");
        var auth = builder.AddProject("auth", repo.Project("services/auth/Auth.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(name: "http")
            .WithHttpHealthCheck("/healthz")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi"));
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc)
                .Routes("app/api/").Authorize(auth, headers: ["x-user"], respond: []));

        var routing = await LocalEdge.RoutingAsync(builder.Resources, protostrap.Resource, 10000,
            (_, _) => Task.FromResult("host.docker.internal:51234"), CancellationToken.None);

        var authorize = JsonDocument.Parse(routing).RootElement.GetProperty("services")[0].GetProperty("authorize");
        Assert.Equal("""{"by":"auth","headers":["x-user"]}""", JsonSerializer.Serialize(authorize));
    }

    [Fact]
    public async Task TheLocalEdgeAuthorizesAsTheAccountsDoes()
    {
        // An open authorizer, its timeout as a Go duration, and what it skips reach the local
        // edge by internal/edge's names for them.
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        var protostrap = builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.dev.localhost");
        var auth = builder.AddProject("auth", repo.Project("services/auth/Auth.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(name: "http")
            .WithHttpHealthCheck("/healthz")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi"));
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc).Routes("app/")
                .Authorize(auth, headers: ["x-user"], open: true, timeout: TimeSpan.FromMilliseconds(500), skip: ["/assets/", "*.js"]));

        var routing = await LocalEdge.RoutingAsync(builder.Resources, protostrap.Resource, 10000,
            (_, _) => Task.FromResult("host.docker.internal:51234"), CancellationToken.None);

        var authorize = JsonDocument.Parse(routing).RootElement.GetProperty("services")[0].GetProperty("authorize");
        Assert.Equal("""{"by":"auth","headers":["x-user"],"open":true,"timeout":"500ms","skip":["/assets/","*.js"]}""", JsonSerializer.Serialize(authorize));
    }

    [Fact]
    public async Task TheEdgeDependsOnTheServicesItReaches()
    {
        // Under Aspire's container tunnel, a project only gets its address on the container
        // network once a container that depends on it is created, which is after that container's
        // start event. So the edge names the services it reaches, as Aspire finds a container's
        // dependencies: each service with routes, and each authorizer of one.
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.dev.localhost");
        var auth = builder.AddProject("auth", repo.Project("services/auth/Auth.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi"));
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc)
                .Route("app/api/", rate: 50).Authorize(auth, path: "/auth/check", headers: ["x-user"], forward: ["cookie"]));
        builder.AddProject("ledger", repo.Project("services/ledger/Ledger.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi"));
        builder.AddProject("worker", repo.Project("services/worker/Worker.csproj"))
            .PublishAsProtoStrapWorker(w => w.Request(cpu: "250m", memory: "256Mi"));
        var edge = Assert.Single(builder.Resources.OfType<ContainerResource>(), r => r.Name == "protostrap-edge");

        var dependencies = await edge.GetResourceDependenciesAsync(
            builder.ExecutionContext, new ResourceDependencyDiscoveryOptions { DiscoveryMode = ResourceDependencyDiscoveryMode.DirectOnly },
            TestContext.Current.CancellationToken);

        Assert.Equal(["api", "auth"], dependencies.Select(r => r.Name).Order());
    }

    [Fact]
    public async Task TheDashboardLinksEachHostOnTheEdge()
    {
        // Envoy answers only the hosts' local hostnames, so the dashboard links each by the
        // host's name. The endpoint's own URL, on localhost, matches no host, so it is left to
        // the details.
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        builder.AddEnvironment()
            .WithHost("web", "protofast.dev", local: "web.dev.localhost")
            .WithHost("admin", "admin.protofast.dev", local: "admin.dev.localhost")
            .WithLocalEdge(port: 21000);
        var edge = Assert.Single(builder.Resources.OfType<ContainerResource>(), r => r.Name == "protostrap-edge");

        // Aspire adds the endpoint's URL before it runs the resource's callbacks.
        var urls = new List<ResourceUrlAnnotation> { new() { Url = "http://localhost:21000", Endpoint = new EndpointReference(edge, "http") } };
        var context = new ResourceUrlsCallbackContext(builder.ExecutionContext, edge, urls, TestContext.Current.CancellationToken);
        foreach (var callback in edge.Annotations.OfType<ResourceUrlsCallbackAnnotation>())
            await callback.Callback(context);

        Assert.Equal(
            [
                ("web", "http://web.dev.localhost:21000", UrlDisplayLocation.SummaryAndDetails),
                ("admin", "http://admin.dev.localhost:21000", UrlDisplayLocation.SummaryAndDetails),
                (null, "http://localhost:21000", UrlDisplayLocation.DetailsOnly),
            ],
            urls.Select(u => (u.DisplayText, u.Url, u.DisplayLocation)));
        Assert.All(urls, u => Assert.Equal("http", u.Endpoint?.EndpointName));
    }

    [Fact]
    public void TheDashboardShowsTheEdgeInFrontOfTheServicesItReaches()
    {
        // The edge references each service with routes and each authorizer of one, once the
        // model is complete, so the dashboard draws it as part of the application.
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.dev.localhost");
        var auth = builder.AddProject("auth", repo.Project("services/auth/Auth.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi"));
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc)
                .Routes("app/api/").Authorize(auth, path: "/auth/check", headers: ["x-user"]));
        builder.AddProject("ledger", repo.Project("services/ledger/Ledger.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi"));
        var edge = Assert.Single(builder.Resources.OfType<ContainerResource>(), r => r.Name == "protostrap-edge");

        LocalEdge.ReferenceReachedServices(builder.Resources, edge);

        Assert.Equal(
            [("api", "Reference"), ("auth", "Reference")],
            edge.Annotations.OfType<ResourceRelationshipAnnotation>().Select(r => (r.Resource.Name, r.Type)).Order());
    }

    [Fact]
    public async Task TheEdgeIsRenderedWhenAspireResolvesItsConfig()
    {
        using var repo = new FixtureRepo();
        // It is `protostrap edge render --routing <file> --out <dir>`.
        var render = repo.WriteExecutable("bin/protostrap", """
            #!/bin/sh
            echo "$*" > "$6/args"
            echo '{}' > "$6/envoy.json"

            """);
        var builder = TestApp.Run(repo);
        builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.dev.localhost").WithCheck(render);
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Route("app/api/", rate: 50));
        var edge = Assert.Single(builder.Resources.OfType<ContainerResource>(), r => r.Name == "protostrap-edge");
        var args = new List<object>();
        var context = new CommandLineArgsCallbackContext(args, edge, TestContext.Current.CancellationToken) { ExecutionContext = builder.ExecutionContext };
        foreach (var callback in edge.Annotations.OfType<CommandLineArgsCallbackAnnotation>())
            await callback.Callback(context);
        var config = Assert.IsType<LocalEdge.Config>(args[args.IndexOf("-c") + 1]);

        // Aspire resolves it once the api has its address on the container network.
        var path = await config.RenderAsync((_, _) => Task.FromResult("aspire.dev.internal:31000"), TestContext.Current.CancellationToken);

        Assert.Equal("/etc/protostrap/envoy.json", path);
        var dir = repo.PathOf("apphost/obj/protostrap-edge");
        Assert.Contains("\"address\": \"aspire.dev.internal:31000\"", File.ReadAllText(Path.Combine(dir, "routing.json")));
        Assert.Equal($"edge render --routing {dir}/routing.json --out {dir}", File.ReadAllText(Path.Combine(dir, "args")).Trim());
        Assert.True(File.Exists(Path.Combine(dir, "envoy.json")));
    }

    private static async Task<Dictionary<string, object>> RunModeEnvironmentAsync(IResource resource)
    {
        var values = new Dictionary<string, object>();
        var context = new EnvironmentCallbackContext(new DistributedApplicationExecutionContext(DistributedApplicationOperation.Run), resource, values);
        foreach (var callback in resource.Annotations.OfType<EnvironmentCallbackAnnotation>())
            await callback.Callback(context);
        return values;
    }
}
