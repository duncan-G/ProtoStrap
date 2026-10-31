using System.Diagnostics;
using System.Text;
using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using Aspire.Hosting.Pipelines;
using Microsoft.Extensions.Logging;

namespace ProtoStrap.Hosting.Spec;

/// <summary>The spec couldn't be written, or `protostrap check` rejected it.</summary>
internal sealed class SpecGenerationException(string message) : Exception(message);

/// <summary>
/// SpecStep is the protostrap-spec pipeline step, which generates the spec, writes it if it
/// changed, and runs `protostrap check` on it.
/// </summary>
internal static class SpecStep
{
    public const string Name = "protostrap-spec";

    public static PipelineStep Create(ProtoStrapEnvironmentResource environment) => new()
    {
        Name = Name,
        Description = "Writes protostrap.yaml from the app model",
        Action = context => RunAsync(environment, context),
        RequiredBySteps = [WellKnownPipelineSteps.Publish],
    };

    private static async Task RunAsync(ProtoStrapEnvironmentResource environment, PipelineStepContext context)
    {
        try
        {
            await WriteAsync(environment, context.Model, context.ExecutionContext, context.Logger, context.CancellationToken);
        }
        catch (SpecGenerationException)
        {
            // Aspire logs a failed step but the AppHost still exits with 0, which would let a
            // drift check that runs it with `dotnet run` pass.
            Environment.ExitCode = 1;
            throw;
        }
    }

    /// <summary>WriteAsync generates the spec and writes it, failing with every problem if it can't.</summary>
    public static async Task<SpecResult> WriteAsync(
        ProtoStrapEnvironmentResource environment,
        DistributedApplicationModel model,
        DistributedApplicationExecutionContext execution,
        ILogger logger,
        CancellationToken cancellationToken)
    {
        var generator = new SpecGenerator(model, execution, environment, environment.SourceTracerFactory);
        var result = await generator.GenerateAsync(cancellationToken);
        foreach (var warning in result.Warnings)
            logger.LogWarning("{Warning}", warning);
        if (result.Problems.Count > 0)
        {
            var message = new StringBuilder($"{result.SpecPath} wasn't written: {result.Problems.Count} problem{(result.Problems.Count == 1 ? "" : "s")}");
            foreach (var problem in result.Problems)
                message.Append("\n  ").Append(problem);
            throw new SpecGenerationException(message.ToString());
        }

        var existing = File.Exists(result.SpecPath) ? await File.ReadAllTextAsync(result.SpecPath, cancellationToken) : null;
        if (existing == result.Yaml)
        {
            logger.LogInformation("{Spec} is up to date", result.SpecPath);
        }
        else
        {
            Directory.CreateDirectory(Path.GetDirectoryName(result.SpecPath)!);
            await File.WriteAllTextAsync(result.SpecPath, result.Yaml, new UTF8Encoding(encoderShouldEmitUTF8Identifier: false), cancellationToken);
            logger.LogInformation("Wrote {Spec}", result.SpecPath);
        }

        await CheckAsync(environment.Binary, environment.CheckCommand, result.SpecPath, logger, cancellationToken);
        return result;
    }

    /// <summary>
    /// CheckAsync runs `protostrap check` on the spec, if protostrap can be found, and warns when
    /// it isn't the AppHost's ProtoStrap.
    /// </summary>
    private static async Task CheckAsync(ProtoStrapBinary binary, string? command, string specPath, ILogger logger, CancellationToken cancellationToken)
    {
        if (command is null)
            return;
        var executable = binary.Find(command);
        if (executable is null)
        {
            logger.LogInformation("{Command} isn't installed, so protostrap check didn't run on {Spec}", command, specPath);
            return;
        }
        if (await binary.MismatchAsync(executable, cancellationToken) is { } mismatch)
            logger.LogWarning("{Mismatch}", mismatch);

        var start = new ProcessStartInfo(executable)
        {
            WorkingDirectory = Path.GetDirectoryName(specPath)!,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            UseShellExecute = false,
        };
        start.ArgumentList.Add("check");
        start.ArgumentList.Add("--spec");
        start.ArgumentList.Add(specPath);
        using var process = Process.Start(start)!;
        var output = process.StandardOutput.ReadToEndAsync(cancellationToken);
        var error = process.StandardError.ReadToEndAsync(cancellationToken);
        await process.WaitForExitAsync(cancellationToken);
        var text = (await output + await error).Trim();
        if (process.ExitCode == 0)
        {
            logger.LogInformation("{Output}", text);
            return;
        }
        var message = new StringBuilder($"protostrap check failed on {specPath}:");
        foreach (var line in text.Split('\n'))
            message.Append("\n  ").Append(line.TrimEnd());
        throw new SpecGenerationException(message.ToString());
    }
}
