namespace ProtoStrap.Hosting.Tests.Support;

/// <summary>
/// FakeAws is an aws CLI in the fixture repository that answers what dev's secret needs: the
/// profiles, a profile's region, whether its SSO session is signed in, sso login, and the
/// secret's value. Files in its state directory say what it answers, and it logs every call.
/// </summary>
internal sealed class FakeAws
{
    private readonly FixtureRepo _repo;
    private readonly string _state;
    private readonly string _log;

    public FakeAws(FixtureRepo repo)
    {
        _repo = repo;
        _state = repo.PathOf("aws-state");
        _log = repo.PathOf("aws.log");
        Directory.CreateDirectory(_state);
        Bin = repo.PathOf("aws-bin");
        repo.WriteExecutable("aws-bin/aws", $$"""
            #!/bin/sh
            echo "$*" >> '{{_log}}'
            d='{{_state}}'
            case "$1 $2" in
              "--version ") echo "aws-cli/2.31.0" ;;
              "configure list-profiles") cat "$d/profiles" 2>/dev/null ;;
              "configure get") if [ -f "$d/region" ]; then cat "$d/region"; else exit 1; fi ;;
              "sts get-caller-identity") if [ -f "$d/signed-in" ]; then echo "123456789012"; else echo "Error when retrieving token from sso: Token has expired and refresh failed" >&2; exit 255; fi ;;
              "sso login") if [ -f "$d/login-fails" ]; then echo "Failed to retrieve an SSO token" >&2; exit 1; fi; touch "$d/signed-in" ;;
              "secretsmanager get-secret-value")
                if [ -f "$d/denied" ]; then echo "An error occurred (AccessDeniedException) when calling the GetSecretValue operation: not authorized" >&2; exit 254; fi
                if [ -f "$d/secret" ]; then cat "$d/secret"; else echo "An error occurred (ResourceNotFoundException) when calling the GetSecretValue operation: Secrets Manager can't find the specified secret." >&2; exit 254; fi ;;
              *) echo "unexpected: $*" >&2; exit 2 ;;
            esac
            """);
        Profiles("developer");
        SignedIn = true;
    }

    /// <summary>Bin is the directory aws is in, for PATH.</summary>
    public string Bin { get; }

    public string[] Calls => File.Exists(_log) ? File.ReadAllLines(_log) : [];

    public void Profiles(params string[] names) => File.WriteAllText(Path.Combine(_state, "profiles"), string.Join('\n', names) + "\n");

    public string? Region { set => SetFile("region", value); }

    public bool SignedIn { set => SetFile("signed-in", value ? "" : null); }

    public bool LoginFails { set => SetFile("login-fails", value ? "" : null); }

    public bool Denied { set => SetFile("denied", value ? "" : null); }

    /// <summary>Secret is the secret's value, as the CLI prints it with --output text, or null if there's none.</summary>
    public string? Secret { set => SetFile("secret", value); }

    private void SetFile(string name, string? contents)
    {
        var path = Path.Combine(_state, name);
        if (contents is null)
            File.Delete(path);
        else
            File.WriteAllText(path, contents);
    }

    /// <summary>
    /// DevSecret is dev's secret for the protofast app with this aws on PATH, and the region in the
    /// environment unless it says otherwise.
    /// </summary>
    public DevSecret DevSecret(string? region = "us-west-2", string? path = null) => new()
    {
        App = "protofast",
        Variable = name => name switch
        {
            "PATH" => path ?? Bin,
            "AWS_REGION" => region,
            _ => null,
        },
    };
}
