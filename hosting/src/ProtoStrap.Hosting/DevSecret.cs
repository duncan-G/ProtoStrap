using System.ComponentModel;
using System.Diagnostics;
using System.Text.Json;
using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.Logging;
using ProtoStrap.Hosting.Spec;

namespace ProtoStrap.Hosting;

/// <summary>
/// DevSecret is dev's secret, &lt;app&gt;/dev (docs/secrets.md#dev): the values a local run
/// takes, in the application's account beside prod's secret and named like it. Under `aspire
/// run` the AppHost reads it with the developer's AWS SSO sign-in, through the AWS CLI, and gives
/// every parameter a ProtoStrap component takes its value from the key the component reads in an
/// account: its prefix and the variable's name, such as Keycloak_KC_BOOTSTRAP_ADMIN_CLIENT_SECRET,
/// or the Shared_ one. Connection strings and their parts stay Aspire's, which point at the local
/// resources. Only the AppHost reads the secret; the components get their values as they always
/// do under `aspire run`.
/// </summary>
internal sealed class DevSecret
{
    /// <summary>DefaultProfile is the AWS profile the AppHost signs in with unless it's told otherwise.</summary>
    public const string DefaultProfile = "developer";

    private const string Shared = "Shared_";

    /// <summary>App is the application's name: the spec's app.</summary>
    public required string App { get; init; }

    /// <summary>Profile is the AWS SSO profile, with the Developer permission set.</summary>
    public string Profile { get; init; } = DefaultProfile;

    /// <summary>Variable reads the environment: PATH, AWS_REGION and AWS_DEFAULT_REGION. Tests replace it.</summary>
    public Func<string, string?> Variable { get; init; } = Environment.GetEnvironmentVariable;

    /// <summary>Name is the secret's: &lt;app&gt;/dev.</summary>
    public string Name => App + "/dev";

    /// <summary>
    /// Use is a parameter a component takes as one of its variables, and the key it reads in an
    /// account.
    /// </summary>
    internal sealed record Use(ParameterResource Parameter, string Component, string Variable, string Key)
    {
        public string SharedKey => Shared + Variable;
    }

    /// <summary>
    /// ApplyAsync is what `aspire run` does before it starts anything: it reads the secret, if any
    /// component takes a parameter, and adds each parameter's value to the AppHost's
    /// configuration, where Aspire reads it when it resolves the parameter.
    /// </summary>
    internal async Task ApplyAsync(IEnumerable<IResource> resources, DistributedApplicationExecutionContext execution,
        IConfigurationBuilder configuration, ILogger logger, CancellationToken cancellationToken)
    {
        var uses = await UsesAsync(resources, execution, cancellationToken);
        if (uses.Count == 0)
            return;
        var region = await SignInAsync(logger, cancellationToken);
        var keys = await ReadAsync(region, cancellationToken);
        var values = Values(uses, keys, Name);
        configuration.AddInMemoryCollection(values.Select(v => KeyValuePair.Create(v.Key, (string?)v.Value)));
        logger.LogInformation("{Count} parameters take their values from {Secret}", values.Count, Name);
    }

    /// <summary>
    /// UsesAsync finds the parameters ProtoStrap components take, from their environments as they
    /// are under `aspire run`, as the spec finds the keys of the environment's secret: a parameter
    /// a variable holds is the key of that variable's name. PostgreSQL's user is left out, as the
    /// spec leaves it out.
    /// </summary>
    internal static async Task<IReadOnlyList<Use>> UsesAsync(IEnumerable<IResource> resources,
        DistributedApplicationExecutionContext execution, CancellationToken cancellationToken)
    {
        var uses = new List<Use>();
        foreach (var resource in resources.OrderBy(r => r.Name, StringComparer.Ordinal))
        {
            if (!resource.TryGetLastAnnotation<ProtoStrapComponentAnnotation>(out var component))
                continue;
            var values = new Dictionary<string, object>(StringComparer.Ordinal);
            var context = new EnvironmentCallbackContext(execution, resource, values, cancellationToken);
            foreach (var callback in resource.Annotations.OfType<EnvironmentCallbackAnnotation>())
                await callback.Callback(context);
            if (resource is PostgresServerResource)
                values.Remove("POSTGRES_USER");

            var prefix = component.Configure().SecretPrefix ?? PascalCase(resource.Name) + "_";
            var connectionDetails = SpecGenerator.ConnectionProperties(values);
            foreach (var (variable, value) in values.OrderBy(v => v.Key, StringComparer.Ordinal))
            {
                var parameter = value switch
                {
                    ParameterResource p => p,
                    IResourceBuilder<ParameterResource> p => p.Resource,
                    _ => null,
                };
                if (parameter is not null && !connectionDetails.Contains(variable))
                    uses.Add(new Use(parameter, resource.Name, variable, prefix + variable));
            }
        }
        return uses;
    }

    /// <summary>
    /// Values gives each parameter its value from the secret's keys, as configuration: its own
    /// key, or the Shared_ one. A parameter two components take must have the same value under
    /// each key. It names every key that is missing or disagrees, never a value.
    /// </summary>
    internal static Dictionary<string, string> Values(IReadOnlyList<Use> uses, IReadOnlyDictionary<string, string> keys, string secret)
    {
        var values = new Dictionary<string, string>(StringComparer.Ordinal);
        var problems = new List<string>();
        foreach (var group in uses.GroupBy(u => u.Parameter).OrderBy(g => g.Key.Name, StringComparer.Ordinal))
        {
            var found = new List<(string Key, string Value)>();
            foreach (var use in group)
            {
                if (keys.TryGetValue(use.Key, out var own))
                    found.Add((use.Key, own));
                else if (keys.TryGetValue(use.SharedKey, out var shared))
                    found.Add((use.SharedKey, shared));
                else
                    problems.Add($"{secret} has no {use.Key} or {use.SharedKey}, which {use.Component} takes as {use.Variable} (parameter {group.Key.Name})");
            }
            var distinct = found.DistinctBy(f => f.Value).ToList();
            if (distinct.Count > 1)
            {
                var names = string.Join(" and ", found.Select(f => f.Key).Distinct().Order(StringComparer.Ordinal));
                problems.Add($"{names} differ in {secret}, but the AppHost gives them one parameter, {group.Key.Name}");
            }
            else if (distinct.Count == 1)
                values[ConfigurationKey(group.Key)] = distinct[0].Value;
        }
        if (problems.Count > 0)
            throw new DevSecretException(
                $"`aspire run` takes its parameters from dev's secret, {secret}:\n  {string.Join("\n  ", problems)}\n" +
                $"Set each key in it with ProtoStrap's scripts/set-secret.sh {secret} <key>=-, or the application's own script.");
        return values;
    }

    /// <summary>
    /// SignInAsync makes sure the profile is signed in, running `aws sso login` when its session
    /// is missing or expired, and returns the region the secret is in.
    /// </summary>
    public async Task<string> SignInAsync(ILogger logger, CancellationToken cancellationToken)
    {
        var aws = FindAws() ?? throw new DevSecretException(
            $"`aspire run` reads dev's secret, {Name}, with the AWS CLI v2, which isn't on PATH. Install it, then sign in once: aws configure sso --profile {Profile}");
        if ((await RunAsync(aws, ["--version"], cancellationToken)).Exit != 0)
            throw new DevSecretException($"The AWS CLI on PATH, {aws}, doesn't run: `aspire run` needs the AWS CLI v2 to read dev's secret, {Name}.");
        var profiles = await RunAsync(aws, ["configure", "list-profiles"], cancellationToken);
        if (!profiles.Output.Split('\n', StringSplitOptions.TrimEntries).Contains(Profile, StringComparer.Ordinal))
            throw new DevSecretException(
                $"`aspire run` reads dev's secret, {Name}, with the AWS profile {Profile}, which isn't configured. " +
                $"Make it once with aws configure sso --profile {Profile}, choosing the application's account and the Developer permission set.");

        var region = await RegionAsync(aws, cancellationToken);
        if (await SignedInAsync(aws, region, cancellationToken))
            return region;
        logger.LogInformation("The AWS SSO session of profile {Profile} is missing or expired: signing in", Profile);
        if (await LoginAsync(aws, cancellationToken) is not 0)
            throw new DevSecretException($"Signing in failed: run aws sso login --profile {Profile}, then aspire run again.");
        if (!await SignedInAsync(aws, region, cancellationToken))
            throw new DevSecretException($"aws sso login --profile {Profile} didn't sign in: run it again, then aspire run again.");
        return region;
    }

    /// <summary>
    /// ReadAsync reads the secret's keys. Its errors never quote the value.
    /// </summary>
    public async Task<IReadOnlyDictionary<string, string>> ReadAsync(string region, CancellationToken cancellationToken)
    {
        var aws = FindAws() ?? throw new DevSecretException("The AWS CLI v2 isn't on PATH.");
        var result = await RunAsync(aws, ["secretsmanager", "get-secret-value", "--secret-id", Name, "--profile", Profile,
            "--region", region, "--query", "SecretString", "--output", "text"], cancellationToken);
        if (result.Exit != 0)
        {
            if (result.Error.Contains("ResourceNotFoundException", StringComparison.Ordinal))
                throw new DevSecretException(
                    $"Dev's secret, {Name}, doesn't exist in {region}, or has no value yet. Create it once with " +
                    $"aws secretsmanager create-secret --name {Name} --region {region}, then fill it with the application's script.");
            if (result.Error.Contains("AccessDenied", StringComparison.Ordinal))
                throw new DevSecretException(
                    $"The AWS profile {Profile} can't read {Name}: sign in to the application's account with the Developer permission set.");
            throw new DevSecretException($"Reading {Name} failed: {result.Error.Trim()}");
        }
        try
        {
            return JsonSerializer.Deserialize<Dictionary<string, string>>(result.Output)
                ?? throw new JsonException();
        }
        catch (JsonException)
        {
            throw new DevSecretException($"{Name} isn't a JSON object of strings, which ProtoStrap's scripts/set-secret.sh writes.");
        }
    }

    private async Task<string> RegionAsync(string aws, CancellationToken cancellationToken)
    {
        foreach (var name in (string[])["AWS_REGION", "AWS_DEFAULT_REGION"])
        {
            if (Variable(name) is { } region && !string.IsNullOrWhiteSpace(region))
                return region.Trim();
        }
        var configured = await RunAsync(aws, ["configure", "get", "region", "--profile", Profile], cancellationToken);
        if (configured.Exit == 0 && configured.Output.Trim() is { Length: > 0 } fromProfile)
            return fromProfile;
        throw new DevSecretException(
            $"The AWS profile {Profile} has no region, so `aspire run` can't find dev's secret, {Name}. " +
            $"Set it once to the region the application's account runs in: aws configure set region <region> --profile {Profile}");
    }

    private async Task<bool> SignedInAsync(string aws, string region, CancellationToken cancellationToken) =>
        (await RunAsync(aws, ["sts", "get-caller-identity", "--profile", Profile, "--region", region, "--output", "text"], cancellationToken)).Exit == 0;

    /// <summary>
    /// LoginAsync runs `aws sso login`, which opens the browser, on the AppHost's own console, and
    /// waits for the developer.
    /// </summary>
    private async Task<int> LoginAsync(string aws, CancellationToken cancellationToken)
    {
        var start = new ProcessStartInfo(aws) { UseShellExecute = false };
        start.ArgumentList.Add("sso");
        start.ArgumentList.Add("login");
        start.ArgumentList.Add("--profile");
        start.ArgumentList.Add(Profile);
        using var process = Process.Start(start)!;
        await process.WaitForExitAsync(cancellationToken);
        return process.ExitCode;
    }

    private string? FindAws() =>
        (Variable("PATH") ?? "")
            .Split(Path.PathSeparator, StringSplitOptions.RemoveEmptyEntries)
            .SelectMany(dir => OperatingSystem.IsWindows() ? [Path.Combine(dir, "aws.exe"), Path.Combine(dir, "aws.cmd")] : new[] { Path.Combine(dir, "aws") })
            .FirstOrDefault(File.Exists);

    private readonly record struct Result(int Exit, string Output, string Error);

    /// <summary>RunAsync runs the CLI without a pager, and gives up after 30 seconds.</summary>
    private static async Task<Result> RunAsync(string aws, string[] args, CancellationToken cancellationToken)
    {
        var start = new ProcessStartInfo(aws)
        {
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            UseShellExecute = false,
        };
        start.Environment["AWS_PAGER"] = "";
        foreach (var arg in args)
            start.ArgumentList.Add(arg);
        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        timeout.CancelAfter(TimeSpan.FromSeconds(30));
        try
        {
            using var process = Process.Start(start)!;
            var output = process.StandardOutput.ReadToEndAsync(timeout.Token);
            var error = process.StandardError.ReadToEndAsync(timeout.Token);
            try
            {
                await process.WaitForExitAsync(timeout.Token);
            }
            catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested)
            {
                process.Kill(entireProcessTree: true);
                throw new DevSecretException($"aws {args[0]} {(args.Length > 1 ? args[1] : "")} took more than 30 seconds.");
            }
            return new Result(process.ExitCode, await output, await error);
        }
        catch (Win32Exception)
        {
            throw new DevSecretException("The AWS CLI v2 on PATH can't be run.");
        }
    }

    /// <summary>
    /// ConfigurationKey is where Aspire reads a parameter's value from configuration: its
    /// ParameterResource.ConfigurationKey, which isn't public.
    /// </summary>
    private static string ConfigurationKey(ParameterResource parameter) =>
        parameter.IsConnectionString ? $"ConnectionStrings:{parameter.Name}" : $"Parameters:{parameter.Name}";

    private static string PascalCase(string name) =>
        string.Concat(name.Split('-', StringSplitOptions.RemoveEmptyEntries).Select(part => char.ToUpperInvariant(part[0]) + part[1..]));
}

/// <summary>DevSecretException stops `aspire run` with what to do about dev's secret.</summary>
internal sealed class DevSecretException(string message) : InvalidOperationException(message);
