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
                .Route("app/api/", rate: 50).Authorize(auth, path: "/auth/check", headers: ["x-user"], forward: ["cookie"]));
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
    public void TheEdgeNeedsProtoStrapOnThePath()
    {
        var problem = Assert.Throws<InvalidOperationException>(() => LocalEdge.Renderer("protostrap-that-isnt-installed"));
        Assert.Contains("protostrap-that-isnt-installed", problem.Message);
        Assert.Contains("go build", problem.Message);
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
