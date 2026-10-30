# The build context is sample/services/dotnet, so build from there:
#   docker build -f Migrate/Dockerfile .
FROM mcr.microsoft.com/dotnet/sdk:10.0 AS build
WORKDIR /src
# Restore first, from the project files alone, so code changes reuse the cached packages.
COPY Directory.Build.props Directory.Packages.props ./
COPY Shared/Shared.csproj Shared/
COPY Migrate/Migrate.csproj Migrate/
RUN dotnet restore Migrate/Migrate.csproj
COPY Shared/ Shared/
COPY Migrate/ Migrate/
RUN dotnet publish Migrate/Migrate.csproj --configuration Release --no-restore --output /app

FROM mcr.microsoft.com/dotnet/runtime:10.0
WORKDIR /app
COPY --from=build /app .
USER $APP_UID
ENTRYPOINT ["dotnet", "Migrate.dll"]
